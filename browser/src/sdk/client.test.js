// client.test.js — 扩展 host 客户端 caps 恒声明与 commands 自答
// 对齐 Go libs/host/client.go buildCommandTable + dispatch.go commandsJSON（§5.1/§5.2）：
//   - caps.exec.commands 恒声明 commands（level=1，desc/help 与 vcore meta.go 同源）
//   - commands 自答输出 {"commands": [{name, desc}]}（{name, desc} 视图，不含 level/help）
import { test } from "node:test";
import assert from "node:assert/strict";
import { AICClient } from "./client.js";

// Node <19 无全局 crypto：用 node:crypto 的 webcrypto shim（对齐 auth.test.js）
if (!globalThis.crypto?.subtle) {
  globalThis.crypto = (await import("node:crypto")).webcrypto;
}
if (typeof globalThis.btoa !== "function") {
  globalThis.btoa = (s) => Buffer.from(s, "binary").toString("base64");
}

// makeClient 构造未连接的客户端（mock caps 发布通道，不触网）。
function makeClient() {
  const client = new AICClient({
    key: "host_test01.1.dGVzdC1zZWNyZXQtMDEyMzQ1Njc4OWFiY2RlZg.uid01",
    onLog: () => {},
  });
  client.registerCommand("browser", 2, async () => ({}), {
    desc: "control a web browser (agent-browser CLI)",
    help: "browser <subcommand> [args...]",
    stateful: true,
    backgroundable: true,
  });
  let published = null;
  client.nc = { publish: (subj, data) => { published = { subj, data }; } };
  client.hostID = "host_test01";
  client.uid = "uid01";
  client.credVer = 1;
  return { client, getPublished: () => published };
}

test("caps: 恒声明 commands（level=1, desc/help 与 vcore meta.go 同源）+ 注册命令", async () => {
  const { client, getPublished } = makeClient();
  await client._publishCaps();

  assert.ok(getPublished(), "caps 应发布");
  const caps = JSON.parse(getPublished().data);
  assert.equal(getPublished().subj, "u.uid01.h.host_test01.1.caps");

  const names = caps.exec.commands.map((c) => c.name);
  assert.deepEqual(names, ["commands", "browser"], "恒声明 commands 应位于注册命令之前");

  const cmd = caps.exec.commands.find((c) => c.name === "commands");
  assert.equal(cmd.level, 1, "commands 基础等级 = Read(1)");
  assert.equal(cmd.desc, "discover available commands on a target");
  assert.match(cmd.help, /^commands\n/);
});

test("commands 自答: {name, desc} 视图（§5.2，不含 level/help）", () => {
  const { client } = makeClient();
  const out = client.commands.get("commands").handler();
  assert.deepEqual(JSON.parse(out.content), {
    commands: [
      { name: "commands", desc: "discover available commands on a target" },
      { name: "browser", desc: "control a web browser (agent-browser CLI)" },
    ],
  });
  assert.deepEqual(out.attrs, { action: "commands" });
});

test("registerCommand 显式覆盖恒声明 commands（保留名）", () => {
  const { client } = makeClient();
  client.registerCommand("commands", 3, async () => ({}), { desc: "custom" });
  assert.equal(client.commands.get("commands").desc, "custom");
  assert.equal(client.commands.get("commands").requiredLevel, 3);
});

// resolveNatsURL/platformURL：与 Go libs/host/natsurl.go 同语义
import { resolveNatsURL, platformURL } from "./client.js";

test("resolveNatsURL: 协议推断与路径前缀", () => {
  assert.equal(resolveNatsURL("https://ivec.ai"), "wss://ivec.ai/api/nc");
  assert.equal(resolveNatsURL("http://localhost:4000"), "ws://localhost:4000/api/nc");
  assert.equal(resolveNatsURL("http://localhost:4000/"), "ws://localhost:4000/api/nc");
  assert.equal(resolveNatsURL("ivec.ai"), "wss://ivec.ai/api/nc");
  assert.equal(resolveNatsURL("ws://localhost:4000"), "ws://localhost:4000/api/nc");
  assert.equal(resolveNatsURL("http://127.0.0.1:4000/rses/aiv"), "ws://127.0.0.1:4000/rses/aiv/api/nc");
});

test("platformURL: http/https 页面入口，保留路径前缀", () => {
  assert.equal(platformURL("https://ivec.ai"), "https://ivec.ai");
  assert.equal(platformURL("http://localhost:4000/"), "http://localhost:4000");
  assert.equal(platformURL("ws://localhost:4000"), "http://localhost:4000");
  assert.equal(platformURL("http://127.0.0.1:4000/rses/aiv"), "http://127.0.0.1:4000/rses/aiv");
});
