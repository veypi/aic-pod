/**
 * client.js — AIC host NATS client for Chrome Extension（指令集 v2）
 *
 * 对齐 Go: libs/host/client.go + dispatch.go（§6.1/§6.2）：
 *   - 连接级 subject（caps/presence）不变：u.{uid}.h.{host_id}.{cred_ver}.caps|presence
 *   - 工具流量会话级：单订阅 u.{uid}.s.*.h.host_{host_id}.>（HostInboxSubject），
 *     subject 解析取 sid/tool，响应经 NATS req-reply 返回
 *   - 请求信封 proto.ToolRequest（HMAC-SHA256，K_tool 派生）；响应 proto.ToolResponse
 *   - caps v2：fs.actions=[read/write/edit]（§4.5：与 page 端同一套 PageFS 代码，
 *     扩展 IndexedDB 后端，按 host_id 寻址）
 *     + exec.commands 统一命令表 = 恒声明 commands（能力发现，§5.1，与 Go
 *       buildCommandTable 对齐）+ registerCommand 注册命令（browser 等）
 * Uses @nats-io/nats-core (via bundled ESM) for NATS over WebSocket.
 */

import { wsconnect, errors as natsErrors } from "../lib/nats/nats-core.js";
import { deriveKeys } from "./crypto.js";
import { generateConnectTokenRaw, verifyToolRequestSig } from "./auth.js";
import { hostInboxSubject, parseToolReqSubject, parseRequest, buildCaps, TOOL_FS, TOOL_EXEC, Level } from "./proto.js";
import { PageFS } from "./page_fs.js";
import { HistoryStore } from "./history.js";

// ---- re-export for handler ----
export { errors as natsErrors } from "../lib/nats/nats-core.js";

const HEARTBEAT_INTERVAL = 20_000; // 20s

// COMMANDS_DECL 恒声明 commands（§5.1：与 Go libs/vcore/meta.go 的 commands 条目同源，
// 禁止另行声明）：desc 输出给 AI（commands），help 由服务端 procs 拦截 `--help` 返回。
// 服务端对 host 的 commands 走应答式转发（§5.2，与 page 同构），扩展端自答。
const COMMANDS_DECL = {
  name: "commands",
  requiredLevel: Level.READ, // 与 Go levels.go execCoreLevels 一致
  desc: "discover available commands on a target",
  help: "commands\n" +
    "  capability discovery: list declared commands (name + desc) of the target;\n" +
    "  use `action --help` for the full help of any command",
};

// FS_REQUIRED 与 Go libs/vcore/levels.go FSRequired 同源（§2.4，禁止各自另写）：
// read/ls/rg=Read(1)，write/edit/cp/mv/rm=Write(2)，未声明 action 兜底 Danger(3)；
// rm recursive 删非空目录动态提升 Danger（_handleFsRequest 里按目录子项检查）。
const FS_REQUIRED = { read: Level.READ, ls: Level.READ, rg: Level.READ, write: Level.WRITE, edit: Level.WRITE, cp: Level.WRITE, mv: Level.WRITE, rm: Level.WRITE };
// FS_ACTIONS caps 声明（与 Go proto.AllFSActions 对齐：全集显式声明，非 null）。
const FS_ACTIONS = ["read", "write", "edit", "ls", "rg", "cp", "mv", "rm"];

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

// handler 超时保护（§2026-08-06）：MV3 SW 生命周期下 pending promise 可能永久丢失，
// 必须在扩展层兜底超时并返回错误（解开 stateful 串行链，防止单条挂起拖死后续指令）。
const HANDLER_TIMEOUT_MS = 50_000;

function withTimeout(p, ms, msg) {
  return new Promise((resolve, reject) => {
    const t = setTimeout(() => reject(new Error(msg)), ms);
    p.then(
      (v) => {
        clearTimeout(t);
        resolve(v);
      },
      (e) => {
        clearTimeout(t);
        reject(e);
      },
    );
  });
}

// resolveNatsURL 由平台地址推导 NATS WebSocket 端点（与 Go libs/host/natsurl.go 同一语义）：
// http→ws、https→wss、ws/wss 保留；无 scheme 补 https；host 可带路径前缀。
export function resolveNatsURL(host) {
  let h = (host || "").trim() || "https://ivec.ai";
  if (!h.includes("://")) h = "https://" + h;
  const u = new URL(h);
  let scheme = u.protocol.slice(0, -1); // 去冒号
  if (scheme === "https") scheme = "wss";
  else if (scheme === "http") scheme = "ws";
  const prefix = u.pathname.replace(/\/+$/, "");
  return `${scheme}://${u.host}${prefix}/aic/api/nc`;
}

// platformURL 由平台地址推导平台页面地址（http/https 页面入口，保留路径前缀）。
export function platformURL(host) {
  let h = (host || "").trim() || "https://ivec.ai";
  if (!h.includes("://")) h = "https://" + h;
  const u = new URL(h);
  let scheme = u.protocol;
  if (scheme === "ws:") scheme = "http:";
  else if (scheme === "wss:") scheme = "https:";
  return `${scheme}//${u.host}${u.pathname.replace(/\/+$/, "")}`;
}

export class AICClient {
  constructor(options) {
    this.opts = options;
    this.nc = null;           // NATS connection
    this.kTool = null;        // K_tool key for request verification
    this.hostID = null;
    this.uid = null;
    this.credVer = 0;
    this.commands = new Map(); // exec 命令表 name → {name, requiredLevel, desc, help, handler, stateful, backgroundable}
    // 恒声明 commands（§5.1，Go buildCommandTable 同构）：能力发现自答。
    // registerCommand("commands", ...) 显式覆盖（保留名，与 page_exec RESERVED 语义一致）。
    this.commands.set("commands", {
      name: COMMANDS_DECL.name,
      requiredLevel: COMMANDS_DECL.requiredLevel,
      desc: COMMANDS_DECL.desc,
      help: COMMANDS_DECL.help,
      handler: () => ({ content: this._commandsJSON(), attrs: { action: "commands" } }),
      stateful: false,
      backgroundable: false,
    });
    this.chains = new Map();   // stateful 命令串行链（对齐 Go vcore/browser mutex 语义）
    this.nonceCache = new Map(); // 防重放：nonce → deadline ms
    this._historyStore = new HistoryStore(); // 执行历史 IndexedDB 持久化（SW 重启保留）
    this._histQ = Promise.resolve(); // 历史写串行队列（保序：add 先于 updateState）
    this.fs = null;            // PageFS 实例（connect 后持有；fs 通道 + browser 截图落盘复用）
    this.heartbeatTimer = null;
    this.reconnecting = false;
    this.closed = false;
    this._connected = false; // 真实连接状态：仅 wsconnect 成功 + caps 发布完成后为 true
    this._reconnectAttempt = 0; // 指数退避计数（重连成功后重置）
    this.logf = options.onLog || ((fmt, ...args) => {
      const ts = new Date().toISOString().slice(11, 19);
      console.log(`[${ts}] ${fmt}`, ...args);
    });
  }

  /**
   * Register an exec command (caps v2 §6.3 统一命令声明表). Must be called before connect().
   * 注册信息 = {name, desc, help, level}：desc 输出给 AI（commands），help 由服务端
   * procs 拦截 `--help` 返回，level 仅供服务端审批判断——均与 Go libs/vcore/meta.go 同源。
   * stateful/backgroundable 为命令内部实现细节（串行链/后台化），不进协议。
   * @param {string} name 命令名（如 "browser"）
   * @param {number} requiredLevel 基础权限等级（§2.4，风险子命令动态提升由 handler 侧判定）
   * @param {(ctx, data) => Promise<{state, content, error, attrs}>} handler
   *        data = exec 负载 {action, argv, workdir?}
   * @param {{desc?: string, help?: string, stateful?: boolean, backgroundable?: boolean}} [opts]
   */
  registerCommand(name, requiredLevel, handler, opts = {}) {
    this.commands.set(name, {
      name,
      requiredLevel,
      desc: opts.desc || "",
      help: opts.help || "",
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

    // fs 后端：与 page 端同一套 PageFS 代码（IndexedDB 本地单根），
    // 扩展 origin 独立 → 与页面 IndexedDB 物理隔离，按 host_id 寻址（§4.5）。
    this.fs = new PageFS();

    this.logf("starting aic-browser v%s [%s/%s] (host=%s)", version, deviceType, deviceName, hostID);

    // Build NATS connection options（端点完全由 host 推断）
    const natsURL = resolveNatsURL(this.opts.host);
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
      // 指数退避（与 aic 页面端 nc.worker.js 对齐）：2s→4s→8s→16s→30s 封顶
      // +0-100ms 抖动；固定 2s 会对不可达服务器高频重连（浪费 + 服务端日志轰炸）。
      reconnectDelayHandler: () => {
        const base = Math.min(2000 * 2 ** this._reconnectAttempt, 30000);
        this._reconnectAttempt++;
        return base + Math.floor(Math.random() * 100);
      },
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
    // 真实连接建立（wsconnect resolve + caps 发布成功）后才标记已连接——
    // 防止错误地址下 wsconnect 无限重连（永不 resolve）时 UI 误报已连接。
    this._connected = true;
  }

  // connected 反映 NATS 连接是否真实建立（首连成功过且未 close）。
  // 断线重连中保持 true（自动恢复），首连失败/未连接为 false。
  get connected() {
    return this._connected;
  }

  /**
   * Close connection gracefully.
   */
  async close() {
    this.closed = true;
    this._connected = false;
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
            this._reconnectAttempt = 0; // 退避计数重置（本次连续重连结束）
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
    const commands = [];
    for (const [, v] of this.commands) {
      const d = { name: v.name, level: v.requiredLevel };
      if (v.desc) d.desc = v.desc;
      if (v.help) d.help = v.help;
      commands.push(d);
    }
    const caps = buildCaps({
      hostID: this.hostID,
      credVer: this.credVer,
      version: this.opts.version || "v0.2.0",
      deviceType: this.opts.deviceType || "browser",
      deviceName: this.opts.deviceName || "Chrome",
      fsActions: FS_ACTIONS, // 扩展接入 PageFS（§4.5：与 page 端同一套代码，按 host_id 寻址）
      commands,
    });

    const subj = `u.${this.uid}.h.${this.hostID}.${this.credVer}.caps`;
    this.nc.publish(subj, JSON.stringify(caps));
    this.logf("caps published to %s (commands=%d)", subj, commands.length);
  }

  /**
   * _commandsJSON 返回本 host 的命令表（§5.2：{name, desc} 视图，与 Go
   * sdk/host/dispatch.go commandsJSON 同构——level 仅供审批判断，help 由服务端
   * procs 拦截 `--help` 返回，均不暴露给 AI）。
   */
  _commandsJSON() {
    const cmds = [];
    for (const [, v] of this.commands) {
      cmds.push({ name: v.name, desc: v.desc });
    }
    return JSON.stringify({ commands: cmds });
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
   * 处理一条工具请求（§6.2 host 端验证规范）：
   * 验签 → deadline 过期拒绝 → nonce 窗口去重 → granted_level 纵深检查 → 分发。
   * subject: u.{uid}.h.host_{host_id}.fs|exec.req.{sid}（§6.1 v4——sid 段定向，
   * sid/tool 从信封读取）。
   */
  async _handleToolRequest(msg) {
    // 校验 subject 形态（sid/tool 从信封读取）
    if (!parseToolReqSubject(msg.subject)) {
      this._respond(msg, { msg_id: "", state: "error", error: `invalid tool request subject: ${msg.subject}` });
      return;
    }

    const req = parseRequest(new TextDecoder().decode(msg.data));
    if (!req) {
      this._respond(msg, { msg_id: "", state: "error", error: "invalid request: malformed JSON" });
      return;
    }
    const sid = req.session_id || "";
    const tool = req.tool;

    // 执行历史：请求解析成功即记录（后续 reject/分发经 _respond 更新状态）
    this._recordHistory({
      time: new Date().toISOString(),
      sid,
      tool,
      action: req.action || "",
      msgId: req.msg_id || "",
      state: "pending",
      error: "",
    });

    this.logf("→ request: %s %s (msg=%s sid=%s)", tool, msg.subject, req.msg_id, sid);

    // 1. 验签
    if (!(await verifyToolRequestSig(req, this.hostID, this.kTool))) {
      this.logf("tool request rejected: invalid/missing K_tool signature for %s", req.msg_id);
      this._respond(msg, { msg_id: req.msg_id, state: "rejected", error: "invalid request signature" });
      return;
    }

    // 2. deadline 必填且未过期（空 = "永不过期"请求，拒绝；格式非法拒绝。
    //    对齐 Go dispatch.go：请求方为可信服务端，恒签发 RFC3339 deadline）
    if (!req.deadline) {
      this._respond(msg, { msg_id: req.msg_id, state: "rejected", error: "missing deadline" });
      return;
    }
    const dlMs = new Date(req.deadline).getTime();
    if (isNaN(dlMs)) {
      this._respond(msg, { msg_id: req.msg_id, state: "rejected", error: "invalid deadline" });
      return;
    }
    const deadlineMs = dlMs;
    if (Date.now() > dlMs) {
      this._respond(msg, { msg_id: req.msg_id, state: "rejected", error: "request expired" });
      return;
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
      await this._handleFsRequest(msg, req, sid);
      return;
    }

    const execData = parseExecData(req.data);
    if (!execData) {
      this._respond(msg, { msg_id: req.msg_id, state: "error", error: "invalid exec data: action is required" });
      return;
    }

    const cmd = this.commands.get(execData.action);
    if (!cmd) {
      this._respond(msg, {
        msg_id: req.msg_id, state: "error",
        error: `exec: unknown action "${execData.action}" (not declared by this host; run commands to discover available commands)`,
      });
      return;
    }

    if (req.granted_level < cmd.requiredLevel) {
      const reason = `exec ${execData.action} requires level ${cmd.requiredLevel} (granted ${req.granted_level})`;
      this.logf("tool request waiting approval: %s (msg=%s)", reason, req.msg_id);
      this._respond(msg, {
        msg_id: req.msg_id, state: "waiting",
        need_approval: { reason },
      });
      return;
    }

    // 5. 分发执行（stateful 命令按 action 串行：Go vcore/browser 以 mutex 保护实例，
    //    JS 扩展为全局单例（多会话共用一组 tab），promise 链等价串行）
    const ctx = {
      grantedLevel: req.granted_level,
      sessionID: sid,
      msgID: req.msg_id,
      fs: this.fs, // PageFS 实例（browser screenshot 等本地产出物落盘，§2.2）
    };

    const run = async () => {
      try {
        // handler 超时保护（2026-08-06）：MV3 SW 在执行中被 Chrome 回收时 pending
        // promise 永不 resolve → 单条挂起会卡死 stateful 串行链，后续所有指令全部
        // 超时。50s 覆盖正常操作（open 15s / wait 30s / eval 30s / download 30s），
        // 超时返回错误并解开串行链（后续指令不再排队）。
        const result = await withTimeout(
          Promise.resolve().then(() => cmd.handler(ctx, execData)),
          HANDLER_TIMEOUT_MS,
          `handler timeout after ${HANDLER_TIMEOUT_MS / 1000}s (service worker likely recycled; command aborted, retry)`,
        );
        this._respond(msg, buildResponse(req.msg_id, result));
      } catch (err) {
        this._respond(msg, { msg_id: req.msg_id, state: "error", error: err.message });
      }
    };

    if (cmd.stateful) {
      await this._enqueue(cmd.name, run);
    } else {
      await run();
    }
  }

  /**
   * _handleFsRequest 处理 fs 工具请求（§4/§6.2）：
   * granted_level 纵深检查（与 vcore.FSRequired 同源）→ PageFS.run 执行。
   * data = fs JSON 参数全体（action/path/offset/limit/content/edits/workdir?）；
   * $SESSION 根绑定 subject 中的 sid。
   */
  async _handleFsRequest(msg, req, sid) {
    let params;
    try {
      params = JSON.parse(req.data);
    } catch (_) {
      params = null;
    }
    if (!params || typeof params !== "object") {
      this._respond(msg, { msg_id: req.msg_id, state: "error", error: "invalid fs data: malformed JSON" });
      return;
    }
    const action = String(params.action || "").toLowerCase();
    let required = FS_REQUIRED[action] ?? Level.DANGER;
    // rm recursive 删非空目录动态提升 Danger（与 Go FSRequiredIn 同源）
    if (action === "rm" && params.recursive && params.path) {
      try {
        const st = await this.fs.stat(params.path);
        if (st && st.dir) {
          const l = await this.fs.list(params.path);
          if ((l.items || []).length > 0) required = Level.DANGER;
        }
      } catch (_) {
        /* 判定失败按 Write，执行路径报真正的错误 */
      }
    }
    if (req.granted_level < required) {
      const reason = `fs ${action || "?"} requires level ${required} (granted ${req.granted_level})`;
      this.logf("fs request waiting approval: %s (msg=%s)", reason, req.msg_id);
      this._respond(msg, {
        msg_id: req.msg_id, state: "waiting",
        need_approval: { reason },
      });
      return;
    }
    try {
      const out = await this.fs.run(params);
      this._respond(msg, buildResponse(req.msg_id, out));
    } catch (err) {
      this._respond(msg, { msg_id: req.msg_id, state: "error", error: err.message });
    }
  }

  /** _enqueue 将任务追加到指定指令的串行链尾（前序失败不阻断后续）。 */
  _enqueue(name, task) {
    const prev = this.chains.get(name) || Promise.resolve();
    const next = prev.then(task, task);
    this.chains.set(name, next.catch(() => {}));
    return next;
  }

  _recordHistory(entry) {
    this._histQ = this._histQ
      .then(() => this._historyStore.add(entry))
      .catch((e) => this.logf("history add failed: %s", e?.message || e));
  }

  // historySnapshot 返回最近 200 条执行历史（options 执行历史页）。
  historySnapshot() {
    return this._historyStore.list(200);
  }

  // historyClear 清空执行历史（options 手动清除）。
  historyClear() {
    return this._historyStore.clear();
  }

  _respond(msg, resp) {
    // 回填终态（pending → completed/error/...），经串行队列保证在 add 之后
    if (resp.msg_id) {
      this._histQ = this._histQ
        .then(() => this._historyStore.updateState(resp.msg_id, resp.state, resp.error))
        .catch((e) => this.logf("history update failed: %s", e?.message || e));
    }
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
