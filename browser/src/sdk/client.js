/**
 * client.js — AIC host NATS client for Chrome Extension（指令集 v2）
 *
 * 对齐 Go SDK: sdk/host/client.go + dispatch.go（§6.1/§6.2）：
 *   - 连接级 subject（caps/presence）不变：u.{uid}.h.{host_id}.{cred_ver}.caps|presence
 *   - 工具流量会话级：单订阅 u.{uid}.s.*.h.host_{host_id}.>（HostInboxSubject），
 *     subject 解析取 sid/tool，响应经 NATS req-reply 返回
 *   - 请求信封 proto.ToolRequest（HMAC-SHA256，K_tool 派生）；响应 proto.ToolResponse
 *   - caps v2：fs.actions=[]（浏览器扩展无文件系统）+ exec.programs=[]（显式纯虚拟）
 *     + exec.virtual 按扩展能力声明（browser 指令）
 * Uses @nats-io/nats-core (via bundled ESM) for NATS over WebSocket.
 */

import { wsconnect, errors as natsErrors } from "../lib/nats/nats-core.js";
import { deriveKeys } from "./crypto.js";
import { generateConnectTokenRaw, verifyToolRequestSig } from "./auth.js";
import { hostInboxSubject, parseToolReqSubject, parseRequest, buildCaps, TOOL_FS, TOOL_EXEC, Level } from "./proto.js";

// ---- re-export for handler ----
export { errors as natsErrors } from "../lib/nats/nats-core.js";

const HEARTBEAT_INTERVAL = 20_000; // 20s

/**
 * Parse credential string: host_id.cred_ver.secret.uid
 */
function parseCredential(cred) {
  const parts = cred.split(".");
  if (parts.length !== 4) {
    throw new Error("invalid credential format: expected <host_id>.<cred_ver>.<secret>.<uid>");
  }
  const hostID = parts[0];
  const credVer = parseInt(parts[1], 10);
  if (isNaN(credVer) || credVer === 0) {
    throw new Error(`invalid cred_ver in credential: ${parts[1]}`);
  }
  return { hostID, credVer, secret: parts[2], uid: parts[3] };
}

export class AICClient {
  constructor(options) {
    this.opts = options;
    this.nc = null;           // NATS connection
    this.kTool = null;        // K_tool key for request verification
    this.hostID = null;
    this.uid = null;
    this.credVer = 0;
    this.virtuals = new Map(); // exec 虚拟指令 name → {requiredLevel, handler, stateful, backgroundable}
    this.chains = new Map();   // stateful 虚拟指令串行链（对齐 Go vcore/browser mutex 语义）
    this.nonceCache = new Map(); // 防重放：nonce → deadline ms
    this.heartbeatTimer = null;
    this.reconnecting = false;
    this.closed = false;
    this.logf = options.onLog || ((fmt, ...args) => {
      const ts = new Date().toISOString().slice(11, 19);
      console.log(`[${ts}] ${fmt}`, ...args);
    });
  }

  /**
   * Register an exec virtual command (caps v2 §6.3). Must be called before connect().
   * @param {string} name 虚拟指令名（如 "browser"）
   * @param {number} requiredLevel 权限等级（§2.4，与 vcore 分级表同源）
   * @param {(ctx, data) => Promise<{state, content, error, attrs}>} handler
   *        data = exec 负载 {action, argv, workdir?}
   * @param {{stateful?: boolean, backgroundable?: boolean}} [opts]
   */
  registerVirtual(name, requiredLevel, handler, opts = {}) {
    this.virtuals.set(name, {
      requiredLevel,
      handler,
      stateful: !!opts.stateful,
      backgroundable: !!opts.backgroundable,
    });
  }

  /**
   * Connect to NATS, publish caps v2, subscribe to session-level inbox, start heartbeat.
   */
  async connect() {
    const { hostID, credVer, secret, uid } = parseCredential(this.opts.key);
    this.hostID = hostID;
    this.uid = uid;
    this.credVer = credVer;

    const version = this.opts.version || "v0.2.0";
    const deviceType = this.opts.deviceType || "browser";
    const deviceName = this.opts.deviceName || "Chrome";

    // Derive keys
    const keys = await deriveKeys(secret, hostID);
    const kConnect = keys.kConnect;
    this.kTool = keys.kTool;

    this.logf("starting aic-browser v%s [%s/%s] (host=%s)", version, deviceType, deviceName, hostID);

    // Build NATS connection options
    let natsURL = (this.opts.url || "wss://ivec.ai/aic/api/nc").trim();
    if (!/^wss?:\/\//i.test(natsURL)) {
      natsURL = "wss://" + natsURL;
    }
    this.logf("NATS URL: %s", natsURL);

    // Validate URL parseable
    try { new URL(natsURL); } catch (e) {
      throw new Error(`invalid NATS URL: "${natsURL}" — ${e.message}`);
    }

    // Pre-generate auth token (Web Crypto is async, cannot be called
    // synchronously from nats tokenAuthenticator)
    const token = await generateConnectTokenRaw(hostID, uid, JSON.stringify({
      agent_version: version,
      device_type: deviceType,
      device_name: deviceName,
    }), kConnect);

    const natsOpts = {
      servers: [natsURL],
      name: `aic-browser-${hostID}`,
      // inboxPrefix 必须落在 u.{uid} 权限域内：默认 _INBOX.* 的 mux 订阅会触发
      // natsauth Permissions Violation，导致所有 request/reply 失效。
      // 与 aic 仓库 nc.worker.js 的修复保持一致。
      inboxPrefix: `u.${uid}._INBOX`,
      token: token,
      reconnect: true,
      maxReconnectAttempts: -1,
      reconnectTimeWait: 2_000,
      pingInterval: 30_000,
      maxPingOut: 3,
      timeout: 10_000,
      ignoreAuthErrorAbort: true,
    };

    try {
      this.nc = await wsconnect(natsOpts);
    } catch (err) {
      throw new Error(`nats connect (${natsURL}): ${err.message}`);
    }

    this.logf("connected to NATS: %s", natsURL);

    // Monitor connection status
    this._setupStatusMonitor();

    // Publish caps v2
    await this._publishCaps();

    // 会话级单订阅（§6.1）：u.{uid}.s.*.h.host_{host_id}.> 覆盖所有会话的 fs/exec 请求
    const inbox = hostInboxSubject(uid, hostID);
    this._sub = this.nc.subscribe(inbox, {
      callback: (err, msg) => {
        if (err) {
          this.logf("subscription error: %s", err.message);
          return;
        }
        this._handleToolRequest(msg);
      },
    });
    this.logf("listening on %s", inbox);

    // Start heartbeat
    this._startHeartbeat();
  }

  /**
   * Close connection gracefully.
   */
  async close() {
    this.closed = true;
    if (this.heartbeatTimer) {
      clearInterval(this.heartbeatTimer);
      this.heartbeatTimer = null;
    }
    if (this.nc) {
      try { await this.nc.close(); } catch (_) { /* ignore */ }
      this.nc = null;
    }
  }

  // ---- private ----

  _setupStatusMonitor() {
    const status = this.nc.status();
    (async () => {
      for await (const s of status) {
        switch (s.type) {
          case "disconnect":
            this.logf("NATS disconnected");
            break;
          case "reconnect":
            this.logf("NATS reconnected, republishing caps");
            this._publishCaps();
            break;
          case "error":
            this.logf("NATS error: %s", s.error?.message || s);
            break;
          case "close":
            this.logf("NATS connection closed");
            break;
        }
      }
    })();
  }

  async _publishCaps() {
    const virtual = [];
    for (const [name, v] of this.virtuals) {
      virtual.push({
        name,
        required_level: v.requiredLevel,
        stateful: v.stateful || undefined,
        backgroundable: v.backgroundable || undefined,
      });
    }
    const caps = buildCaps({
      hostID: this.hostID,
      credVer: this.credVer,
      version: this.opts.version || "v0.2.0",
      deviceType: this.opts.deviceType || "browser",
      deviceName: this.opts.deviceName || "Chrome",
      fsActions: [],      // 浏览器扩展无文件系统（§6.3：[] = 不支持 fs）
      programs: [],       // 显式纯虚拟（§6.3：[] = 无程序）
      virtual,
    });

    const subj = `u.${this.uid}.h.${this.hostID}.${this.credVer}.caps`;
    this.nc.publish(subj, JSON.stringify(caps));
    this.logf("caps published to %s (virtual=%d)", subj, virtual.length);
  }

  _startHeartbeat() {
    this.heartbeatTimer = setInterval(() => {
      if (this.closed || !this.nc || this.nc.isClosed()) return;
      const presence = {
        host_id: this.hostID,
        credential_ver: this.credVer,
        running: 1,
        sent_at: new Date().toISOString(),
      };
      const subj = `u.${this.uid}.h.${this.hostID}.${this.credVer}.presence`;
      this.nc.publish(subj, JSON.stringify(presence));
    }, HEARTBEAT_INTERVAL);
  }

  /**
   * 处理一条会话级工具请求（§6.2 host 端验证规范）：
   * 验签 → deadline 过期拒绝 → nonce 窗口去重 → granted_level 纵深检查 → 分发。
   * subject: u.{uid}.s.{sid}.h.host_{host_id}.fs|exec.req
   */
  async _handleToolRequest(msg) {
    // 从 subject 解析 sid/tool（会话隔离：bg 命名空间与 topic 一致）
    const parsed = parseToolReqSubject(msg.subject);
    if (!parsed) {
      this._respond(msg, { msg_id: "", state: "error", error: `invalid tool request subject: ${msg.subject}` });
      return;
    }
    const { sid, tool } = parsed;

    const req = parseRequest(new TextDecoder().decode(msg.data));
    if (!req) {
      this._respond(msg, { msg_id: "", state: "error", error: "invalid request: malformed JSON" });
      return;
    }

    this.logf("→ request: %s %s (msg=%s sid=%s)", tool, msg.subject, req.msg_id, sid);

    // 1. 验签
    if (!(await verifyToolRequestSig(req, this.hostID, this.kTool))) {
      this.logf("tool request rejected: invalid/missing K_tool signature for %s", req.msg_id);
      this._respond(msg, { msg_id: req.msg_id, state: "rejected", error: "invalid request signature" });
      return;
    }

    // 2. deadline 过期拒绝（非空但不可解析 → 拒绝，防"永不过期"请求）
    let deadlineMs = 0;
    if (req.deadline) {
      const dl = new Date(req.deadline).getTime();
      if (isNaN(dl)) {
        this._respond(msg, { msg_id: req.msg_id, state: "rejected", error: "invalid deadline" });
        return;
      }
      deadlineMs = dl;
      if (Date.now() > dl) {
        this._respond(msg, { msg_id: req.msg_id, state: "rejected", error: "request expired" });
        return;
      }
    }

    // 3. nonce 必填且窗口内缓存去重（空 nonce 直接拒绝，防跳过去重）
    if (!req.nonce) {
      this._respond(msg, { msg_id: req.msg_id, state: "rejected", error: "missing nonce" });
      return;
    }
    const now = Date.now();
    for (const [n, dl] of this.nonceCache) {
      if (now > dl) this.nonceCache.delete(n);
    }
    if (this.nonceCache.has(req.nonce)) {
      this._respond(msg, { msg_id: req.msg_id, state: "rejected", error: "duplicate nonce" });
      return;
    }
    this.nonceCache.set(req.nonce, deadlineMs || now + 600_000);

    // 4. granted_level 纵深检查（§2.4：host 端按 caps 声明再自检，不足 waiting 转审批）
    if (tool === TOOL_FS) {
      // fs.actions=[] 声明（不支持）：不进入 granted 判定，直接拒绝
      this._respond(msg, {
        msg_id: req.msg_id, state: "error",
        error: "fs not supported on this host (browser extension exposes exec virtual commands only)",
      });
      return;
    }

    const execData = parseExecData(req.data);
    if (!execData) {
      this._respond(msg, { msg_id: req.msg_id, state: "error", error: "invalid exec data: action is required" });
      return;
    }

    const virt = this.virtuals.get(execData.action);
    if (!virt) {
      const supported = [...this.virtuals.keys()].join(", ") || "(none)";
      this._respond(msg, {
        msg_id: req.msg_id, state: "error",
        error: `exec: unknown command "${execData.action}" (supported virtual commands on this host: ${supported})`,
      });
      return;
    }

    if (req.granted_level < virt.requiredLevel) {
      const reason = `exec ${execData.action} requires level ${virt.requiredLevel} (granted ${req.granted_level})`;
      this.logf("tool request waiting approval: %s (msg=%s)", reason, req.msg_id);
      this._respond(msg, {
        msg_id: req.msg_id, state: "waiting",
        need_approval: { reason },
      });
      return;
    }

    // 5. 分发执行（stateful 虚拟指令按 action 串行：Go vcore/browser 以 mutex 保护实例，
    //    JS 扩展为全局单例（多会话共用一组 tab），promise 链等价串行）
    const ctx = {
      grantedLevel: req.granted_level,
      sessionID: sid,
      msgID: req.msg_id,
    };

    const run = async () => {
      try {
        const result = await virt.handler(ctx, execData);
        this._respond(msg, buildResponse(req.msg_id, result));
      } catch (err) {
        this._respond(msg, { msg_id: req.msg_id, state: "error", error: err.message });
      }
    };

    if (virt.stateful) {
      await this._enqueue(virt.name, run);
    } else {
      await run();
    }
  }

  /** _enqueue 将任务追加到指定指令的串行链尾（前序失败不阻断后续）。 */
  _enqueue(name, task) {
    const prev = this.chains.get(name) || Promise.resolve();
    const next = prev.then(task, task);
    this.chains.set(name, next.catch(() => {}));
    return next;
  }

  _respond(msg, resp) {
    const data = JSON.stringify(resp);
    msg.respond(new TextEncoder().encode(data));
    const logAttrs = { ...resp.attrs };
    if (logAttrs.image_data) {
      logAttrs.image_data = `<base64 ${logAttrs.image_data.length} chars>`;
    }
    const logContent = resp.content ? (resp.content.length > 100 ? resp.content.slice(0, 100) + "..." : resp.content) : "";
    this.logf("← response: msg=%s state=%s content=%q attrs=%j error=%q", resp.msg_id, resp.state, logContent, logAttrs, resp.error);
  }
}

/**
 * parseExecData 解析 exec 负载：{action, argv, workdir?}。
 * data 为信封内 JSON 字符串。返回 null 表示结构非法。
 */
function parseExecData(rawData) {
  try {
    const p = JSON.parse(rawData);
    if (!p || typeof p !== "object") return null;
    const action = String(p.action || "").trim();
    if (!action) return null;
    return {
      action,
      argv: Array.isArray(p.argv) ? p.argv.map(String) : [],
      workdir: typeof p.workdir === "string" ? p.workdir : undefined,
    };
  } catch (_) {
    return null;
  }
}

/**
 * buildResponse 将 handler 结果映射为响应信封（§6.2 错误模型）。
 * handler 返回 {state?, content?, error?, attrs?}：
 *   state 缺省：error 非空 → error，否则 completed。
 */
function buildResponse(msgID, result) {
  const resp = {
    msg_id: msgID,
    content: result.content || "",
    error: result.error || "",
    attrs: result.attrs || {},
  };
  switch (result.state) {
    case "rejected": resp.state = "rejected"; break;
    case "waiting": resp.state = "waiting"; break;
    default:
      resp.state = result.error ? "error" : "completed";
  }
  if (result.need_approval) resp.need_approval = result.need_approval;
  return resp;
}
