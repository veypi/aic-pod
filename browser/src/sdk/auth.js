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
    agent_version: version,
    device_type: deviceType,
    device_name: deviceName,
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
  nonce, deadline, toolData, grantedLevel) {
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
  // payload matches Go struct field declaration order
  return JSON.stringify(payload);
}

export async function verifyToolRequestSig(req, hostID, kTool) {
  const expected = await canonicalToolReqSigInput(
    hostID, req.msg_id, req.session_id, req.tool_name,
    req.nonce, req.deadline, req.tool_data, req.granted_level
  );
  const expectedSig = await hmacSHA256B64(kTool, expected);
  return expectedSig === req.sig;
}
