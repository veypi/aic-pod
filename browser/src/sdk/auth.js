/**
 * auth.js — 连接 token 生成 + 工具请求验签（指令集 v2）
 *
 * Ported from Go SDK: sdk/proto/sign.go
 * canonical 输入与 Go 侧位级一致（JSON 字段序即签名输入序，禁止各自另写）。
 * 固定向量见 sdk/auth.test.js（与 Go vectors_test.go 同源）。
 */

import { hmacSHA256B64, sha256Hex } from "./crypto.js";

// ---- helpers ----

function btoaUrl(bytes) {
  return btoa(String.fromCharCode(...bytes))
    .replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
}

/**
 * Generate e1 connect token: e1.<hostID>.<envInfoB64>.<tsMs>.<nonceB64>.<sigB64>
 * host_info 键序与 Go json.Marshal(map) 对齐（字典序）：agent_version, device_name, device_type。
 */
export function generateConnectToken(hostID, uid, version, deviceType, deviceName, kConnect) {
  const hostInfo = JSON.stringify({
    agent_version: version,
    device_name: deviceName,
    device_type: deviceType,
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

/**
 * canonicalToolReqSigInput 构造工具请求签名的 canonical 输入（§6.2 v2）：
 * {"version":2,"host_id":...,"msg_id":...,"session_id":...,"tool":...,
 *  "data_sha256":...,"granted_level":...,"nonce":...,"deadline":...}
 *
 * req.data 是信封中的原始 JSON 字符串（服务端 json.Marshal(data) 的字节），
 * data_sha256 = sha256(原始字节) 的 hex —— 禁止 JSON.parse 后重序列化。
 */
export async function canonicalToolReqSigInput(req, hostID) {
  const dataHash = await sha256Hex(req.data);
  const payload = {
    version: 2,
    host_id: hostID,
    msg_id: req.msg_id,
    session_id: req.session_id,
    tool: req.tool,
    data_sha256: dataHash,
    granted_level: req.granted_level,
    nonce: req.nonce,
    deadline: req.deadline,
  };
  // payload matches Go struct field declaration order (toolReqSigPayload)
  return JSON.stringify(payload);
}

export async function verifyToolRequestSig(req, hostID, kTool) {
  if (!req || !req.sig || typeof req.data !== "string") return false;
  const expected = await canonicalToolReqSigInput(req, hostID);
  const expectedSig = await hmacSHA256B64(kTool, expected);
  return expectedSig === req.sig;
}
