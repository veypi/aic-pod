// proto.test.js — 协议层固定向量（与 Go sdk/proto/subject_test.go 同源）
import { test } from "node:test";
import assert from "node:assert/strict";
import {
  hostSeg, toolReqSubject, hostInboxSubject, parseToolReqSubject,
  buildCaps, TOOL_EXEC, TOOL_FS, HOST_CLOUD, HOST_PAGE,
} from "./proto.js";

// ---- hostSeg ----

test("hostSeg: 物理 host 加前缀，保留字/已带前缀原样（幂等）", () => {
  assert.equal(hostSeg("vec01"), "host_vec01");
  assert.equal(hostSeg("host_vec01"), "host_vec01");
  assert.equal(hostSeg(HOST_CLOUD), "cloud");
  assert.equal(hostSeg(HOST_PAGE), "page");
});

// ---- ToolReqSubject（§6.1 固定向量） ----

test("toolReqSubject: 物理 host 自动加前缀", () => {
  assert.equal(toolReqSubject("u_vec", "s_abc", "vec01", TOOL_EXEC), "u.u_vec.s.s_abc.h.host_vec01.exec.req");
  assert.equal(toolReqSubject("u_vec", "s_abc", "vec01", TOOL_FS), "u.u_vec.s.s_abc.h.host_vec01.fs.req");
});

test("toolReqSubject: page/cloud 保留字原样", () => {
  assert.equal(toolReqSubject("u_vec", "s_abc", HOST_PAGE, TOOL_EXEC), "u.u_vec.s.s_abc.h.page.exec.req");
  assert.equal(toolReqSubject("u_vec", "s_abc", HOST_CLOUD, TOOL_EXEC), "u.u_vec.s.s_abc.h.cloud.exec.req");
});

test("toolReqSubject: 非法 segment/tool 报错", () => {
  assert.throws(() => toolReqSubject("u..x", "s1", "h1", TOOL_EXEC));
  assert.throws(() => toolReqSubject("u1", "s1", "h1", "browser"), /invalid tool/);
});

// ---- HostInboxSubject ----

test("hostInboxSubject: 会话级单订阅", () => {
  assert.equal(hostInboxSubject("u_vec", "vec01"), "u.u_vec.s.*.h.host_vec01.>");
});

// ---- ParseToolReqSubject ----

test("parseToolReqSubject: 解析还原裸 host_id", () => {
  assert.deepEqual(
    parseToolReqSubject("u.u_vec.s.s_abc.h.host_vec01.exec.req"),
    { uid: "u_vec", sid: "s_abc", host: "vec01", tool: "exec" }
  );
});

test("parseToolReqSubject: page 保留字原样", () => {
  assert.deepEqual(
    parseToolReqSubject("u.u_vec.s.s_abc.h.page.fs.req"),
    { uid: "u_vec", sid: "s_abc", host: "page", tool: "fs" }
  );
});

test("parseToolReqSubject: 非法 subject 返回 null", () => {
  assert.equal(parseToolReqSubject(""), null);
  assert.equal(parseToolReqSubject("u.u1.s.s1.h.h1.exec"), null); // 缺 req 段
  assert.equal(parseToolReqSubject("u.u1.s.s1.h.h1.browser.req"), null); // 非 fs|exec
  assert.equal(parseToolReqSubject("x.u1.s.s1.h.h1.exec.req"), null);
});

// ---- buildCaps（caps v2 三形态） ----

test("buildCaps: browser 扩展形态（fs=[] 不支持、programs=[] 纯虚拟）", () => {
  const caps = buildCaps({
    hostID: "vec01", credVer: 1, version: "v0.4.0",
    deviceType: "browser", deviceName: "Chrome",
    fsActions: [], programs: [],
    virtual: [{ name: "browser", required_level: 2, stateful: true, backgroundable: true }],
  });
  assert.deepEqual(caps.fs, { actions: [] });
  assert.deepEqual(caps.exec.programs, []);
  assert.deepEqual(caps.exec.virtual, [
    { name: "browser", required_level: 2, stateful: true, backgroundable: true },
  ]);
  assert.equal(caps.host_id, "vec01");
  assert.equal(caps.credential_ver, 1);
  assert.equal(caps.agent_version, "v0.4.0");
  assert.equal(caps.device_info.os, "Chrome");
  assert.equal(caps.device_info.arch, "browser");
  assert.equal(typeof caps.device_info.num_cpu, "number");
});

test("buildCaps: null 形态（全部 fs action / 程序不限制）", () => {
  const caps = buildCaps({
    hostID: "h1", credVer: 1, version: "v0.4.0",
    fsActions: null, programs: null, virtual: [],
  });
  assert.equal(JSON.stringify(caps.fs), '{"actions":null}');
  assert.equal(JSON.stringify(caps.exec.programs), "null");
});
