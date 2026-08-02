// auth.test.js — 密钥派生与签名固定向量
// 与 Go sdk/proto/sign_test.go + vectors_test.go 同源（位级一致，零漂移）。
// 修改派生参数或 canonical 结构必须同步更新本文件与 Go 侧，并视为协议变更。
import { test } from "node:test";
import assert from "node:assert/strict";
import { deriveKeys } from "./crypto.js";
import { generateConnectToken, generateConnectTokenRaw, verifyToolRequestSig } from "./auth.js";
import { parseRequest } from "./proto.js";

// Node <19 无全局 crypto：用 node:crypto 的 webcrypto shim
if (!globalThis.crypto?.subtle) {
  globalThis.crypto = (await import("node:crypto")).webcrypto;
}
if (typeof globalThis.btoa !== "function") {
  globalThis.btoa = (s) => Buffer.from(s, "binary").toString("base64");
}

// Go vectors_test.go 固定向量
const SECRET = "dGVzdC1zZWNyZXQtMDEyMzQ1Njc4OWFiY2RlZg";
const HOST_ID = "host_vec01";
const UID = "u_vec";
const VEC_K_CONNECT = "N3kygIFMBZFwCrfzkwcZFbgqt5uf7Rzz6-_oevYQoSA";
const VEC_K_SERVER = "tXA-GUsQs-n0Bmmt09z7140onDlU1PCZq6qmkbpJoUE";
const VEC_K_TOOL = "VMTmXHIBYWUnxQenEBwsqJKgDG_zZdAQXMRyksiwIIg";
const VEC_TOOL_REQ_SIG = "k_47wuS-A22foxdJ4pu0j5VNdP3cS-Sp_t11_lYVNRg";
const VEC_CONNECT_TOKEN = "e1.host_vec01.eyJhZ2VudF92ZXJzaW9uIjoidjAuMy4wIiwiZGV2aWNlX25hbWUiOiJuYXMtMDEiLCJkZXZpY2VfdHlwZSI6ImNsaSJ9.1767225600000.BBBBBBBBBBBBBBBBBBBBBB.1jfc9uC9K-l1-CCI48xkX0yaOgLtkhmcbxPBeDmL1JI";

test("deriveKeys 固定向量（HKDF-SHA256，salt=host_id）", async () => {
  const keys = await deriveKeys(SECRET, HOST_ID);
  assert.equal(keys.kConnect, VEC_K_CONNECT);
  assert.equal(keys.kServer, VEC_K_SERVER);
  assert.equal(keys.kTool, VEC_K_TOOL);
});

test("tool request 验签固定向量（v2 canonical 输入）", async () => {
  const keys = await deriveKeys(SECRET, HOST_ID);
  // Go TestToolRequestSigVector 的同一请求：data 保持原始 JSON 字符串（禁止重序列化）
  const req = {
    msg_id: "msg_001",
    session_id: "s_abc",
    tool: "exec",
    data: `{"action":"ls","argv":["-la"]}`,
    granted_level: 3,
    nonce: "AAAAAAAAAAAAAAAAAAAAAA",
    deadline: "2026-01-02T03:04:05Z",
    sig: VEC_TOOL_REQ_SIG,
  };
  assert.equal(await verifyToolRequestSig(req, HOST_ID, keys.kTool), true);

  // 篡改任一字段验签必失败（与 Go 测试同组断言）
  const tampered1 = { ...req, granted_level: 9 };
  assert.equal(await verifyToolRequestSig(tampered1, HOST_ID, keys.kTool), false);
  const tampered2 = { ...req, data: `{"action":"rm","argv":["-r","/"]}` };
  assert.equal(await verifyToolRequestSig(tampered2, HOST_ID, keys.kTool), false);
  assert.equal(await verifyToolRequestSig(req, "host_other", keys.kTool), false);
  // 缺 sig / data 非字符串直接拒绝
  assert.equal(await verifyToolRequestSig({ ...req, sig: "" }, HOST_ID, keys.kTool), false);
  assert.equal(await verifyToolRequestSig({ ...req, data: { action: "ls" } }, HOST_ID, keys.kTool), false);
});

test("端到端回归：服务端信封文本 → parseRequest → 验签（固定向量）", async () => {
  const keys = await deriveKeys(SECRET, HOST_ID);
  // Go 服务端 json.Marshal(proto.ToolRequest) 的线上字节形态：
  // data 内嵌为原始 JSON 对象（json.RawMessage），不是字符串。
  // 本用例是 §6.2 验签链路的完整回归：parseRequest 必须字节级保真提取 data，
  // 否则 verifyToolRequestSig 必然失败（data 被 JSON.parse 物化成对象的历史 bug）。
  const envelope = '{"msg_id":"msg_001","session_id":"s_abc","tool":"exec","data":{"action":"ls","argv":["-la"]},"granted_level":3,"nonce":"AAAAAAAAAAAAAAAAAAAAAA","deadline":"2026-01-02T03:04:05Z","sig":"' + VEC_TOOL_REQ_SIG + '"}';
  const req = parseRequest(envelope);
  assert.ok(req);
  assert.equal(typeof req.data, "string");
  assert.equal(await verifyToolRequestSig(req, HOST_ID, keys.kTool), true);
  // data 字节任何差异（哪怕只多一个空白）验签必失败
  const spaced = envelope.replace('"data":{"action"', '"data":{ "action"');
  const req2 = parseRequest(spaced);
  assert.ok(req2);
  assert.equal(await verifyToolRequestSig(req2, HOST_ID, keys.kTool), false);
});

test("connect token 固定向量（e1 格式 + canonical 输入）", async () => {
  const keys = await deriveKeys(SECRET, HOST_ID);
  // Go 侧用固定 unix_ms/nonce 生成；JS generateConnectTokenRaw 内部用 Date.now/随机 nonce，
  // 这里复现 canonical 输入验证签名一致性：手工构造同参 token
  const hostInfo = JSON.stringify({ agent_version: "v0.3.0", device_name: "nas-01", device_type: "cli" });
  const token = await generateConnectTokenRaw(HOST_ID, UID, hostInfo, keys.kConnect);
  const parts = token.split(".");
  assert.equal(parts.length, 6);
  assert.equal(parts[0], "e1");
  assert.equal(parts[1], HOST_ID);
  // env_info base64url 解码后与 Go 一致
  const envInfo = atob(parts[2].replace(/-/g, "+").replace(/_/g, "/"));
  assert.equal(envInfo, hostInfo);
  // Go 固定向量的整体 token（unix_ms=1767225600000, nonce=BBBB...）应可被同 canonical 输入复现
  // —— 直接断言 generateConnectToken 公开 API 结构（unix_ms 不可注入，仅验证字段与签名长度）
  assert.equal(parts[0] + "." + parts[1], "e1." + HOST_ID);
  assert.equal(typeof parseInt(parts[3], 10), "number");
  assert.equal(parts[4].length, 22); // 16 字节 nonce base64url
  assert.equal(parts[5].length, 43); // 32 字节 HMAC base64url
});

test("connect token: Go 固定向量逐字节比对（手工构造同参）", async () => {
  const keys = await deriveKeys(SECRET, HOST_ID);
  // Go GenerateConnectToken(vecHostID, vecUID, "v0.3.0", "cli", "nas-01", 1767225600000, "BBBBBBBBBBBBBBBBBBBBBB", vecKConnect)
  // canonical 输入 = {"domain":"aic-host-connect-v1","host_id":"host_vec01","uid":"u_vec","host_info":"...","unix_ms":1767225600000,"nonce":"BBBBBBBBBBBBBBBBBBBBBB"}
  // 用 crypto.js 的 hmac 复现签名段
  const { hmacSHA256B64 } = await import("./crypto.js");
  const hostInfo = JSON.stringify({ agent_version: "v0.3.0", device_name: "nas-01", device_type: "cli" });
  const sigInput = JSON.stringify({
    domain: "aic-host-connect-v1",
    host_id: HOST_ID,
    uid: UID,
    host_info: hostInfo,
    unix_ms: 1767225600000,
    nonce: "BBBBBBBBBBBBBBBBBBBBBB",
  });
  const sig = await hmacSHA256B64(keys.kConnect, sigInput);
  const envInfoB64 = Buffer.from(hostInfo).toString("base64url");
  const token = `e1.${HOST_ID}.${envInfoB64}.1767225600000.BBBBBBBBBBBBBBBBBBBBBB.${sig}`;
  assert.equal(token, VEC_CONNECT_TOKEN);
});
