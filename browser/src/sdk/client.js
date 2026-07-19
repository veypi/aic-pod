/**
 * client.js — AIC NATS client for Chrome Extension
 *
 * Ported from Go SDK: sdk/client.go
 * Uses @nats-io/nats-core (via bundled ESM) for NATS over WebSocket.
 */

import { wsconnect, errors as natsErrors } from "../lib/nats/nats-core.js";
import { deriveKeys } from "./crypto.js";
import { generateConnectTokenRaw, verifyToolRequestSig } from "./auth.js";

// ---- re-export for handler ----
export { errors as natsErrors } from "../lib/nats/nats-core.js";

const HEARTBEAT_INTERVAL = 20_000; // 20s

/**
 * Parse credential string: env_id.cred_ver.secret.uid
 */
function parseCredential(cred) {
  const parts = cred.split(".");
  if (parts.length !== 4) {
    throw new Error("invalid credential format: expected <env_id>.<cred_ver>.<secret>.<uid>");
  }
  const envID = parts[0];
  const credVer = parseInt(parts[1], 10);
  if (isNaN(credVer) || credVer === 0) {
    throw new Error(`invalid cred_ver in credential: ${parts[1]}`);
  }
  return { envID, credVer, secret: parts[2], uid: parts[3] };
}

export class AICClient {
  constructor(options) {
    this.opts = options;
    this.nc = null;           // NATS connection
    this.kTool = null;        // K_tool key for request verification
    this.envID = null;
    this.uid = null;
    this.credVer = 0;
    this.tools = new Map();   // name → Tool
    this.heartbeatTimer = null;
    this.reconnecting = false;
    this.closed = false;
    this.logf = options.onLog || ((fmt, ...args) => {
      const ts = new Date().toISOString().slice(11, 19);
      console.log(`[${ts}] ${fmt}`, ...args);
    });
  }

  /**
   * Register a tool. Must be called before connect().
   */
  registerTool(def, handler) {
    this.tools.set(def.name, { def, handler });
  }

  /**
   * Connect to NATS, publish CAPS, subscribe to tool requests, start heartbeat.
   */
  async connect() {
    const { envID, credVer, secret, uid } = parseCredential(this.opts.key);
    this.envID = envID;
    this.uid = uid;
    this.credVer = credVer;

    const version = this.opts.version || "0.1.0";
    const deviceType = this.opts.deviceType || "browser";
    const deviceName = this.opts.deviceName || "Chrome";

    // Derive keys
    const keys = await deriveKeys(secret, envID);
    const kConnect = keys.kConnect;
    this.kTool = keys.kTool;

    this.logf("starting aic-browser v%s [%s/%s] (env=%s)", version, deviceType, deviceName, envID);

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
    const token = await generateConnectTokenRaw(envID, uid, JSON.stringify({
      env_version: version,
      env_type: deviceType,
      env_name: deviceName,
    }), kConnect);

    const natsOpts = {
      servers: [natsURL],
      name: `aic-browser-${envID}`,
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

    // Publish CAPS
    await this._publishCaps(version, deviceType, deviceName);

    // Subscribe to tool requests
    const toolWildcard = `u.${uid}.e.${envID}.${credVer}.tool.*.req`;
    this._sub = this.nc.subscribe(toolWildcard, {
      callback: (err, msg) => {
        if (err) {
          this.logf("subscription error: %s", err.message);
          return;
        }
        this._handleToolRequest(msg);
      },
    });
    this.logf("listening on %s", toolWildcard);

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
            this._publishCaps(
              this.opts.version || "0.1.0",
              this.opts.deviceType || "browser",
              this.opts.deviceName || "Chrome"
            );
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

  async _publishCaps(version, deviceType, deviceName) {
    const toolDefs = [];
    for (const [_, t] of this.tools) {
      toolDefs.push({
        name: t.def.name,
        description: t.def.description,
        parameters: t.def.parameters,
        required_level: t.def.requiredLevel,
        policy_version: t.def.policyVersion || "1",
      });
    }

    const caps = {
      env_id: this.envID,
      agent_version: version,
      credential_ver: this.credVer,
      device_type: deviceType,
      device_name: deviceName,
      device_info: {
        hostname: deviceName,
        os: "Chrome",
        arch: "browser",
        num_cpu: navigator.hardwareConcurrency || 1,
        go_version: `JS/${version}`,
      },
      tools: toolDefs,
    };

    const subj = `u.${this.uid}.e.${this.envID}.${this.credVer}.caps`;
    this.nc.publish(subj, JSON.stringify(caps));
    this.logf("caps published to %s (%d tools)", subj, toolDefs.length);
  }

  _startHeartbeat() {
    this.heartbeatTimer = setInterval(() => {
      if (this.closed || !this.nc || this.nc.isClosed()) return;
      const presence = {
        env_id: this.envID,
        credential_ver: this.credVer,
        running: 1,
        sent_at: new Date().toISOString(),
      };
      const subj = `u.${this.uid}.e.${this.envID}.${this.credVer}.presence`;
      this.nc.publish(subj, JSON.stringify(presence));
    }, HEARTBEAT_INTERVAL);
  }

  async _handleToolRequest(msg) {
    let req;
    try {
      req = JSON.parse(new TextDecoder().decode(msg.data));
    } catch (err) {
      this._respond(msg, { msg_id: "", status: "error", error: "invalid request: " + err.message });
      return;
    }

    this.logf("→ request: %s %j deadline=%s (msg=%s)", req.tool_name, req.tool_data, req.deadline, req.msg_id);

    // Verify signature
    if (!req.sig) {
      this.logf("tool request rejected: missing K_tool signature for %s", req.tool_name);
      this._respond(msg, { msg_id: req.msg_id, status: "rejected", error: "missing request signature" });
      return;
    }

    if (!(await verifyToolRequestSig(req, this.envID, this.kTool))) {
      this.logf("tool request rejected: invalid K_tool signature for %s", req.tool_name);
      this._respond(msg, { msg_id: req.msg_id, status: "rejected", error: "invalid request signature" });
      return;
    }

    // Check deadline
    if (req.deadline) {
      const dl = new Date(req.deadline).getTime();
      if (Date.now() > dl) {
        this._respond(msg, { msg_id: req.msg_id, status: "rejected", error: "request deadline exceeded" });
        return;
      }
    }

    // Find tool
    const tool = this.tools.get(req.tool_name);
    if (!tool) {
      this.logf("tool request: unknown tool %s", req.tool_name);
      this._respond(msg, { msg_id: req.msg_id, status: "error", error: `unknown tool: ${req.tool_name}` });
      return;
    }

    // Check permission level
    if (req.granted_level < tool.def.requiredLevel && !req.approval) {
      this.logf("tool request denied: %s (granted=%d < required=%d)",
        req.tool_name, req.granted_level, tool.def.requiredLevel);
      this._respond(msg, {
        msg_id: req.msg_id, status: "rejected",
        error: `insufficient permission: ${req.tool_name} requires level ${tool.def.requiredLevel}, got ${req.granted_level}`,
      });
      return;
    }

    // Build request context
    const ctx = {
      grantedLevel: req.granted_level,
      approved: !!req.approval,
      resolvedBy: req.approval ? req.approval.resolved_by : "",
      sessionID: req.session_id,
      msgID: req.msg_id,
    };

    // Execute handler
    try {
      const result = await tool.handler(ctx, req.tool_data);

      const resp = {
        msg_id: req.msg_id,
        content: result.content || "",
        error: result.error || "",
        attrs: result.attrs || {},
      };

      switch (result.status) {
        case "rejected": resp.status = "rejected"; break;
        case "waiting": resp.status = "waiting"; break;
        default:
          resp.status = result.error ? "error" : "completed";
      }

      this._respond(msg, resp);
    } catch (err) {
      this._respond(msg, { msg_id: req.msg_id, status: "error", error: err.message });
    }
  }

  _respond(msg, resp) {
    const data = JSON.stringify(resp);
    msg.respond(new TextEncoder().encode(data));
    const logAttrs = { ...resp.attrs };
    if (logAttrs.image_data) {
      logAttrs.image_data = `<base64 ${logAttrs.image_data.length} chars>`;
    }
    const logContent = resp.content ? (resp.content.length > 100 ? resp.content.slice(0, 100) + "..." : resp.content) : "";
    this.logf("← response: msg=%s status=%s content=%q attrs=%j error=%q", resp.msg_id, resp.status, logContent, logAttrs, resp.error);
  }
}
