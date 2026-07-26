/**
 * auth.js — Token generation for NATS connection and tool request verification
 *
 * Ported from Go SDK: sdk/auth.go
 */

import { hmacSHA256B64, sha256Hex } from "./crypto.js";

// ---- helpers ----

function btoaUrl(bytes) {
  return btoa(String.fromCharCode(...bytes))
    .replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
}

/**
 * JSON.stringify with sorted keys, matching Go's json.Marshal behavior.
 * Required for signature verification parity with the Go server.
 */
function jsonStringifySorted(obj) {
  if (obj === null || obj === undefined) return "null";
  if (typeof obj !== "object") return JSON.stringify(obj);
  if (Array.isArray(obj)) {
    return "[" + obj.map(jsonStringifySorted).join(",") + "]";
  }
  const keys = Object.keys(obj).sort();
  const pairs = keys.map(k => JSON.stringify(k) + ":" + jsonStringifySorted(obj[k]));
  return "{" + pairs.join(",") + "}";
}

/**
 * Generate e1 connect token: e1.<hostID>.<envInfoB64>.<tsMs>.<nonceB64>.<sigB64>
 */
export function generateConnectToken(hostID, uid, version, deviceType, deviceName, kConnect) {
  const hostInfo = JSON.stringify({
    env_version: version,
    env_type: deviceType,
    env_name: deviceName,
  });
  return generateConnectTokenRaw(hostID, uid, hostInfo, kConnect);
}

/**
 * Generate e1 token with pre-serialized hostInfo.
 */
export async function generateConnectTokenRaw(hostID, uid, hostInfo, kConnect) {
  const envInfoB64 = btoaUrl(new TextEncoder().encode(hostInfo));
  const unixMs = Date.now();
  const nonce = crypto.getRandomValues(new Uint8Array(16));
  const nonceB64 = btoaUrl(nonce);

  const sigInput = canonicalConnectSigInput(hostID, uid, hostInfo, unixMs, nonceB64);
  const sigB64 = await hmacSHA256B64(kConnect, sigInput);

  return `e1.${hostID}.${envInfoB64}.${unixMs}.${nonceB64}.${sigB64}`;
}

function canonicalConnectSigInput(hostID, uid, hostInfo, unixMs, nonceB64) {
  // Go uses struct field order, not sorted keys — match field declaration order
  return JSON.stringify({
    domain: "aic-host-connect-v1",
    host_id: hostID,
    uid: uid,
    host_info: hostInfo,
    unix_ms: unixMs,
    nonce: nonceB64,
  });
}

export async function canonicalToolReqSigInput(hostID, msgID, sessionID, toolName,
  nonce, deadline, toolData, grantedLevel, approvalFingerprint) {
  const toolDataJSON = jsonStringifySorted(toolData);
  const toolDataHash = await sha256Hex(toolDataJSON);

  const payload = {
    version: 1,
    host_id: hostID,
    msg_id: msgID,
    session_id: sessionID,
    tool_name: toolName,
    tool_data_sha256: toolDataHash,
    granted_level: grantedLevel,
    nonce: nonce,
    deadline: deadline,
  };
  if (approvalFingerprint !== null && approvalFingerprint !== undefined) {
    payload.approval_fingerprint = approvalFingerprint;
  }
  // payload matches Go struct field declaration order
  return JSON.stringify(payload);
}

export async function verifyToolRequestSig(req, hostID, kTool) {
  const afp = req.approval ? req.approval.fingerprint : undefined;
  const expected = await canonicalToolReqSigInput(
    hostID, req.msg_id, req.session_id, req.tool_name,
    req.nonce, req.deadline, req.tool_data, req.granted_level, afp
  );
  const expectedSig = await hmacSHA256B64(kTool, expected);
  return expectedSig === req.sig;
}

// ---- 审批指纹重算（§10.1 第 4 条） ----

/**
 * JCS（RFC 8785）字符串序列化：最简转义，与 aic types.writeJCSString 一致。
 */
function writeJCSString(s) {
  let out = '"';
  for (const ch of s) {
    const code = ch.codePointAt(0);
    switch (ch) {
      case '"': out += '\\"'; break;
      case "\\": out += "\\\\"; break;
      case "\b": out += "\\b"; break;
      case "\f": out += "\\f"; break;
      case "\n": out += "\\n"; break;
      case "\r": out += "\\r"; break;
      case "\t": out += "\\t"; break;
      default:
        if (code < 0x20) {
          out += "\\u" + code.toString(16).padStart(4, "0");
        } else {
          out += ch;
        }
    }
  }
  return out + '"';
}

/**
 * 计算 {target, action, argv} 的 JCS 规范化 JSON 的 sha256 前 16 位 hex，
 * 与 aic types.ApprovalInputHash 逐字节一致。
 */
export async function approvalInputHash(target, action, argv) {
  let canonical = "{" + writeJCSString("action") + ":" + writeJCSString(action || "");
  canonical += "," + writeJCSString("argv") + ":" + "[" + (argv || []).map(writeJCSString).join(",") + "]";
  if (target) {
    canonical += "," + writeJCSString("target") + ":" + writeJCSString(target);
  }
  canonical += "}";
  const hex = await sha256Hex(canonical);
  return hex.slice(0, 16);
}

/**
 * 按 §2.3 公式重算审批指纹并比对（纵深防御，不只信任签名信封内的审批声明）。
 * fingerprint = fp:{sessionID}:{tool}:{policyVersion}:{hash16}
 */
export async function verifyApprovalFingerprint(fingerprint, sessionID, toolName, hostID, toolData) {
  const parts = String(fingerprint || "").split(":");
  if (parts.length !== 5 || parts[0] !== "fp") return false;
  if (parts[1] !== sessionID || parts[2] !== toolName) return false;
  const action = toolData?.action || "";
  const argv = Array.isArray(toolData?.argv) ? toolData.argv.map(String) : [];
  const hash = await approvalInputHash(hostID, action, argv);
  return parts[4] === hash;
}
