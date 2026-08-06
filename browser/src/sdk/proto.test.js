// proto.test.js — 协议层固定向量（与 Go libs/proto/subject_test.go 同源）
import { test } from "node:test";
import assert from "node:assert/strict";
import {
  hostSeg, toolReqSubject, hostInboxSubject, parseToolReqSubject,
  buildCaps, parseRequest, TOOL_EXEC, TOOL_FS, HOST_CLOUD, HOST_PAGE,
} from "./proto.js";

// ---- hostSeg ----

test("hostSeg: 物理 host 加前缀，保留字/已带前缀原样（幂等）", () => {
  assert.equal(hostSeg("vec01"), "host_vec01");
  assert.equal(hostSeg("host_vec01"), "host_vec01");
  assert.equal(hostSeg(HOST_CLOUD), "cloud");
  assert.equal(hostSeg(HOST_PAGE), "page");
});

// ---- ToolReqSubject（§6.1 v4 会话定向固定向量，与 Go subject_test.go 同源） ----

test("toolReqSubject: 物理 host 自动加前缀", () => {
  assert.equal(toolReqSubject("u_vec", "vec01", TOOL_EXEC, "s9"), "u.u_vec.h.host_vec01.exec.req.s9");
  assert.equal(toolReqSubject("u_vec", "vec01", TOOL_FS, "s9"), "u.u_vec.h.host_vec01.fs.req.s9");
});

test("toolReqSubject: page/cloud 保留字原样", () => {
  assert.equal(toolReqSubject("u_vec", HOST_PAGE, TOOL_EXEC, "s9"), "u.u_vec.h.page.exec.req.s9");
  assert.equal(toolReqSubject("u_vec", HOST_CLOUD, TOOL_EXEC, "s9"), "u.u_vec.h.cloud.exec.req.s9");
});

test("toolReqSubject: 非法 segment/tool 报错", () => {
  assert.throws(() => toolReqSubject("u..x", "h1", TOOL_EXEC, "s9"));
  assert.throws(() => toolReqSubject("u1", "h1", TOOL_EXEC, "")); // 空 sid
  assert.throws(() => toolReqSubject("u1", "h1", "browser", "s9"), /invalid tool/);
});

// ---- HostInboxSubject（连接级单订阅） ----

test("hostInboxSubject: 连接级单订阅", () => {
  assert.equal(hostInboxSubject("u_vec", "vec01"), "u.u_vec.h.host_vec01.>");
});

// ---- ParseToolReqSubject（7 段，§6.1 v4） ----

test("parseToolReqSubject: 解析还原裸 host_id", () => {
  assert.deepEqual(
    parseToolReqSubject("u.u_vec.h.host_vec01.exec.req.s9"),
    { uid: "u_vec", host: "vec01", tool: "exec", sid: "s9" }
  );
});

test("parseToolReqSubject: page 保留字原样 + manual 占位段", () => {
  assert.deepEqual(
    parseToolReqSubject("u.u_vec.h.page.fs.req.manual"),
    { uid: "u_vec", host: "page", tool: "fs", sid: "manual" }
  );
});

test("parseToolReqSubject: 非法 subject 返回 null", () => {
  assert.equal(parseToolReqSubject(""), null);
  assert.equal(parseToolReqSubject("u.u1.h.h1.exec.req"), null); // 缺 sid 段（6 段旧形态）
  assert.equal(parseToolReqSubject("u.u1.h.h1.browser.req.s9"), null); // 非 fs|exec
  assert.equal(parseToolReqSubject("x.u1.h.h1.exec.req.s9"), null);
});

// ---- buildCaps（caps v2 统一命令表） ----

test("buildCaps: browser 扩展形态（fs=[] 不支持、命令表声明 browser）", () => {
  const caps = buildCaps({
    hostID: "vec01", credVer: 1, version: "v0.4.0",
    deviceType: "browser", deviceName: "Chrome",
    fsActions: [],
    commands: [{ name: "browser", desc: "control a web browser", help: "browser <subcommand>...", level: 2 }],
  });
  assert.deepEqual(caps.fs, { actions: [] });
  assert.deepEqual(caps.exec.commands, [
    { name: "browser", desc: "control a web browser", help: "browser <subcommand>...", level: 2 },
  ]);
  assert.equal(caps.host_id, "vec01");
  assert.equal(caps.credential_ver, 1);
  assert.equal(caps.agent_version, "v0.4.0");
  assert.equal(caps.device_info.os, "Chrome");
  assert.equal(caps.device_info.arch, "browser");
  assert.equal(typeof caps.device_info.num_cpu, "number");
});

test("buildCaps: fs null 形态（全部 fs action）", () => {
  const caps = buildCaps({
    hostID: "h1", credVer: 1, version: "v0.4.0",
    fsActions: null, commands: [],
  });
  assert.equal(JSON.stringify(caps.fs), '{"actions":null}');
  assert.deepEqual(caps.exec.commands, []);
});

// ---- parseRequest（§6.2：data 字段字节级保真，验签输入） ----

test("parseRequest: data 保持原始 JSON 字符串（禁止重序列化）", () => {
  // Go 服务端 json.Marshal(proto.ToolRequest) 的字节形态：data 内嵌为原始对象
  const text = '{"msg_id":"m1","session_id":"s1","tool":"exec","data":{"action":"ls","argv":["-la"]},"granted_level":3,"nonce":"n","deadline":"d","sig":"s"}';
  const req = parseRequest(text);
  assert.ok(req);
  assert.equal(req.msg_id, "m1");
  assert.equal(typeof req.data, "string");
  assert.equal(req.data, '{"action":"ls","argv":["-la"]}');
});

test("parseRequest: data 含空白/嵌套/转义时原样保留", () => {
  // data 值带格式化空白、嵌套括号、含转义引号与 } 的字符串——提取边界必须精确
  const text = '{"msg_id":"m1","data": { "a\\"b}": [1, "x\\\\", ",}y", {"n": null, "s": "}"}] } , "sig":"s"}';
  const req = parseRequest(text);
  assert.ok(req);
  assert.equal(req.data, '{ "a\\"b}": [1, "x\\\\", ",}y", {"n": null, "s": "}"}] }');
  // 提取结果必须可被 JSON.parse 还原为等值结构
  assert.deepEqual(JSON.parse(req.data), { 'a"b}': [1, "x\\", ",}y", { n: null, s: "}" }] });
});

test("parseRequest: data 为字符串/数字/数组等任意 JSON 值", () => {
  assert.equal(parseRequest('{"data":"str"}').data, '"str"');
  assert.equal(parseRequest('{"data":123}').data, "123");
  assert.equal(parseRequest('{"data":[1,{"a":2}]}').data, '[1,{"a":2}]');
  assert.equal(parseRequest('{"data":null}').data, "null");
});

test("parseRequest: 非法 JSON / 缺 data 成员返回 null", () => {
  assert.equal(parseRequest(""), null);
  assert.equal(parseRequest("not json"), null);
  assert.equal(parseRequest("[1,2]"), null); // 非对象
  assert.equal(parseRequest('{"msg_id":"m1"}'), null); // 缺 data
  assert.equal(parseRequest('{"data":{"a":1'), null); // 截断
});
