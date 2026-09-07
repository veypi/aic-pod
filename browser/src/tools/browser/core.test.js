/**
 * core.test.js — browser core 的 mock-adapter 单测（node:test）
 *
 * mock adapter 语义：evalIn 直接在当前进程执行 func(...args)（需要页面环境的
 * 用例预置 global.window/__aic_network_logs 等最小桩；snapshot 等 DOM 重操作
 * 以 canned 返回值代替执行，验证 core 侧的解析/状态机/输出格式）。
 */

import { test, beforeEach } from "node:test";
import assert from "node:assert/strict";

import { createBrowserHandler } from "./core.js";

// ---- mock adapter ----

function makeAdapter(overrides = {}) {
  const state = {
    tabs: [], // {id, windowId, title, url, active}
    nextTabId: 1,
    windows: [{ id: 100, incognito: false }],
    store: {},
    settings: { incognito: false, background: true },
    evalResults: [], // 队列：evalIn 依次弹出；空则直接执行 func
    calls: { update: [], create: [], remove: [], cdpSend: [] },
    onUpdatedFns: new Set(),
  };

  const adapter = {
    _state: state,
    tabs: {
      async active() {
        const t = state.tabs.find((t) => t.active);
        if (!t) throw new Error("no active tab");
        return t;
      },
      async get(id) {
        const t = state.tabs.find((t) => t.id === id);
        if (!t) throw new Error(`tab ${id} not found`);
        return t;
      },
      async list(windowId) {
        if (windowId === undefined) return state.tabs;
        return state.tabs.filter((t) => t.windowId === windowId);
      },
      async create({ windowId = 100, url = "about:blank", active = false }) {
        const t = { id: state.nextTabId++, windowId, title: "", url, active };
        state.tabs.push(t);
        return t;
      },
      async update(id, props) {
        state.calls.update.push({ id, props });
        const t = state.tabs.find((t) => t.id === id);
        Object.assign(t, props);
        if (props.url) {
          // 模拟加载完成
          setTimeout(() => {
            for (const fn of state.onUpdatedFns) fn(id, { status: "complete" });
          }, 0);
        }
        return t;
      },
      async remove(id) {
        state.calls.remove.push(id);
        state.tabs = state.tabs.filter((t) => t.id !== id);
      },
      onUpdated: {
        add(fn) { state.onUpdatedFns.add(fn); },
        remove(fn) { state.onUpdatedFns.delete(fn); },
      },
    },
    windows: {
      async listNormal() { return state.windows; },
      async get(id) {
        const w = state.windows.find((w) => w.id === id);
        if (!w) throw new Error(`window ${id} not found`);
        return w;
      },
      async createIncognitoUnfocused() {
        const w = { id: 200, incognito: true };
        state.windows.push(w);
        return w;
      },
    },
    async evalIn(tabId, func, args) {
      const canned = state.evalResults.shift();
      if (canned !== undefined) return canned;
      return func(...args);
    },
    cdp: {
      async attach() {},
      async send(tabId, method, params) {
        state.calls.cdpSend.push({ tabId, method, params });
        if (method === "Runtime.evaluate") return { result: { value: state.cdpEvalValue } };
        if (method === "Page.captureScreenshot") {
          return { data: Buffer.from("fake-jpeg").toString("base64") };
        }
        return {};
      },
      async detach() {},
    },
    downloads: {
      onceCreated() { return () => {}; },
      async searchComplete() { return []; },
    },
    store: {
      async get(key) { return state.store[key] ?? null; },
      async set(key, value) { state.store[key] = value; },
    },
    async settings() { return state.settings; },
    ...overrides,
  };
  return adapter;
}

const ctx = { grantedLevel: 9, sessionID: "s1", msgID: "m1", fs: null };

beforeEach(() => {
  delete global.window;
});

// ---- 入口与解析 ----

test("缺少子命令报错并列出支持集", async () => {
  const h = createBrowserHandler(makeAdapter());
  const r = await h(ctx, { argv: [] });
  assert.equal(r.state, "error");
  assert.match(r.error, /subcommand is required/);
  assert.match(r.error, /snapshot/);
});

test("未知子命令报 unknown action（支持集提示 eval 兜底）", async () => {
  const h = createBrowserHandler(makeAdapter());
  const r = await h(ctx, { argv: ["type", "#a", "x"] });
  assert.equal(r.state, "error");
  assert.match(r.error, /unknown action "type"/);
  assert.match(r.error, /eval <js>/);
});

test("flag 解析错误透传（未知 flag）", async () => {
  const h = createBrowserHandler(makeAdapter());
  const r = await h(ctx, { argv: ["snapshot", "--bogus"] });
  assert.equal(r.state, "error");
  assert.match(r.error, /unknown flag|--bogus/);
});

// ---- open ----

test("open 拒绝非 http(s) URL", async () => {
  const h = createBrowserHandler(makeAdapter());
  const r = await h(ctx, { argv: ["open", "ftp://x"] });
  assert.equal(r.state, "error");
  assert.match(r.error, /only http/);
});

test("open 在工作区创建 tab 并导航（不激活）", async () => {
  const ad = makeAdapter();
  const h = createBrowserHandler(ad);
  const r = await h(ctx, { argv: ["open", "https://example.com"] });
  assert.equal(r.state, undefined); // 成功无 state 字段
  assert.match(r.content, /^✓ /);
  const upd = ad._state.calls.update[0];
  assert.equal(upd.props.url, "https://example.com");
  // 工作区 tab 持久化
  assert.ok(ad._state.store.aicWorker?.tabId);
  assert.equal(ad._state.store.aicWorker.winIncognito, false);
});

// ---- click / @ref 代次 ----

test("click 缺选择器报错；CSS 选择器路径走 css 定位", async () => {
  const ad = makeAdapter();
  const h = createBrowserHandler(ad);
  let r = await h(ctx, { argv: ["click"] });
  assert.match(r.error, /selector or @ref is required/);

  // 预建工作区 tab + 页面变化后 @ref 找不到 → stale
  await h(ctx, { argv: ["open", "https://example.com"] });
  ad._state.evalResults.push({ stale: true }); // 页内未找到当前代次 ref
  r = await h(ctx, { argv: ["click", "@e2"] });
  assert.match(r.error, /stale ref @e2/);
});

test("snapshot 后 click @e1 携带当前代次 ref", async () => {
  const ad = makeAdapter();
  const h = createBrowserHandler(ad);
  await h(ctx, { argv: ["open", "https://example.com"] });
  // snapshot：canned 返回
  ad._state.evalResults.push({ lines: ["- button \"Go\" [ref=e1]"], count: 1 });
  const snap = await h(ctx, { argv: ["snapshot"] });
  assert.match(snap.content, /^✓ /);
  assert.equal(snap.attrs.rows, "1");
  // click：验证注入的 loc 带 gen
  let seenLoc = null;
  ad._state.evalResults.push(undefined); // 弹出标记位（走 func 执行路径）
  const origEval = ad.evalIn;
  ad.evalIn = async (tabId, func, args) => { seenLoc = args[0]; return { ok: true }; };
  const r = await h(ctx, { argv: ["click", "@e1"] });
  assert.match(r.content, /✓ Clicked @e1/);
  assert.equal(seenLoc.ref, "1:e1"); // 第一代 snapshot
  void origEval;
});

// ---- get ----

test("get title/url 从 tab 对象读取；未知子命令报错", async () => {
  const ad = makeAdapter();
  const h = createBrowserHandler(ad);
  await h(ctx, { argv: ["open", "https://example.com"] });
  const tab = ad._state.tabs[0];
  tab.title = "Example";
  let r = await h(ctx, { argv: ["get", "title"] });
  assert.equal(r.content, "Example");
  r = await h(ctx, { argv: ["get", "bogus"] });
  assert.match(r.error, /unknown get sub-command/);
});

// ---- network ----

test("network 列表：window.__aic_network_logs 过滤与截断", async () => {
  global.window = {
    __aic_network_logs: [
      { id: "r1", method: "GET", url: "https://a.com/x", type: "fetch", status: 200 },
      { id: "r2", method: "POST", url: "https://a.com/y", type: "xhr", status: 404 },
    ],
  };
  const ad = makeAdapter();
  const h = createBrowserHandler(ad);
  await h(ctx, { argv: ["open", "https://example.com"] });

  let r = await h(ctx, { argv: ["network"] });
  assert.match(r.content, /\[r1\] GET https:\/\/a\.com\/x \(fetch\) 200/);
  assert.equal(r.attrs.rows, "2");

  r = await h(ctx, { argv: ["network", "--method", "post"] });
  assert.equal(r.attrs.rows, "1");
  assert.match(r.content, /\[r2\] POST/);

  r = await h(ctx, { argv: ["network", "r1"] });
  assert.match(r.content, /"id": "r1"/);

  r = await h(ctx, { argv: ["network", "--clear"] });
  assert.match(r.content, /cleared/);
  assert.deepEqual(global.window.__aic_network_logs, []);
});

// ---- read ----

test("read 无 url：页面 outerHTML 提取可读文本", async () => {
  const ad = makeAdapter();
  ad._state.evalResults.push(undefined);
  ad.evalIn = async () => "<html><body><h1>Hi</h1><p>World</p><script>x()</script></body></html>";
  const h = createBrowserHandler(ad);
  await h(ctx, { argv: ["open", "https://example.com"] });
  const r = await h(ctx, { argv: ["read"] });
  assert.match(r.content, /Hi\n\s*World/);
  assert.equal(r.attrs.truncated, "false");
});

test("read 超限截断（100KB）", async () => {
  const ad = makeAdapter();
  ad.evalIn = async () => "<p>" + "x".repeat(200 * 1024) + "</p>";
  const h = createBrowserHandler(ad);
  await h(ctx, { argv: ["open", "https://example.com"] });
  const r = await h(ctx, { argv: ["read"] });
  assert.equal(r.attrs.truncated, "true");
  assert.ok(new TextEncoder().encode(r.content).length <= 100 * 1024 + 20);
});

// ---- screenshot ----

test("screenshot 走 CDP 并落 ctx.fs", async () => {
  const puts = [];
  const myCtx = {
    ...ctx,
    fs: { put: async (p, blob) => { puts.push({ p, size: blob.size }); return { path: p, bytes: blob.size }; } },
  };
  const ad = makeAdapter();
  const h = createBrowserHandler(ad);
  await h(ctx, { argv: ["open", "https://example.com"] });
  const r = await h(myCtx, { argv: ["screenshot"] });
  assert.equal(r.attrs.action, "screenshot");
  assert.match(r.attrs.path, /^\/screenshot\/screenshot-.*\.jpg$/);
  assert.equal(puts.length, 1);
  assert.equal(puts[0].size, 9); // "fake-jpeg"
  assert.equal(ad._state.calls.cdpSend.at(-1).method, "Page.captureScreenshot");
});

test("screenshot 无 fs 后端报错", async () => {
  const ad = makeAdapter();
  const h = createBrowserHandler(ad);
  await h(ctx, { argv: ["open", "https://example.com"] });
  const r = await h(ctx, { argv: ["screenshot"] });
  assert.match(r.error, /fs backend not available/);
});

// ---- eval ----

test("eval 走 CDP Runtime.evaluate（awaitPromise/returnByValue）", async () => {
  const ad = makeAdapter();
  ad._state.cdpEvalValue = 42;
  const h = createBrowserHandler(ad);
  await h(ctx, { argv: ["open", "https://example.com"] });
  const r = await h(ctx, { argv: ["eval", "1+1"] });
  assert.equal(r.content, "42");
  const send = ad._state.calls.cdpSend.at(-1);
  assert.equal(send.method, "Runtime.evaluate");
  assert.equal(send.params.awaitPromise, true);
});

// ---- tab（工作区模式） ----

test("tab new/list/N/close 限定工作区窗口", async () => {
  const ad = makeAdapter();
  const h = createBrowserHandler(ad);
  await h(ctx, { argv: ["open", "https://example.com"] });
  let r = await h(ctx, { argv: ["tab", "new"] });
  assert.match(r.content, /✓ Opened AI tab/);
  r = await h(ctx, { argv: ["tab", "list"] });
  assert.equal(r.attrs.rows, "2");
  assert.match(r.content, /→ 2\t/); // current 移到新 tab
  r = await h(ctx, { argv: ["tab", "1"] });
  assert.match(r.content, /Switched to AI tab 1/);
  r = await h(ctx, { argv: ["tab", "close", "1"] });
  assert.match(r.content, /✓ Closed AI tab/);
  // 关闭后工作区状态清空
  assert.equal(ad._state.store.aicWorker, null);
});

test("tab close 最后一张拒绝且非错误", async () => {
  const ad = makeAdapter();
  const h = createBrowserHandler(ad);
  await h(ctx, { argv: ["open", "https://example.com"] });
  const r = await h(ctx, { argv: ["tab", "close"] });
  assert.equal(r.attrs.closed, "false");
  assert.match(r.content, /Cannot close last AI tab/);
});

// ---- wait / sleep ----

test("wait 毫秒形态；sleep 解析", async () => {
  const h = createBrowserHandler(makeAdapter());
  let r = await h(ctx, { argv: ["wait", "10"] });
  assert.match(r.content, /✓ wait condition met: 10ms/);
  r = await h(ctx, { argv: ["sleep", "10ms"] });
  assert.match(r.content, /✓ slept for 10ms/);
  r = await h(ctx, { argv: ["sleep", "bogus"] });
  assert.match(r.error, /invalid duration/);
});

test("wait --text 命中页面文本", async () => {
  const ad = makeAdapter();
  const h = createBrowserHandler(ad);
  await h(ctx, { argv: ["open", "https://example.com"] });
  ad.evalIn = async (tabId, func, args) => func(...args);
  global.window = {};
  global.document = { body: { innerText: "hello world" } };
  const r = await h(ctx, { argv: ["wait", "--text", "hello"] });
  assert.match(r.content, /✓ wait condition met: text hello/);
  delete global.document;
});

// ---- 协作模式 ----

test("协作模式（background=false）操作当前激活 tab", async () => {
  const ad = makeAdapter();
  ad._state.settings = { incognito: false, background: false };
  ad._state.tabs.push({ id: 9, windowId: 100, title: "UserTab", url: "https://u.com", active: true });
  const h = createBrowserHandler(ad);
  const r = await h(ctx, { argv: ["get", "title"] });
  assert.equal(r.content, "UserTab");
  // 不创建任何新 tab
  assert.equal(ad._state.calls.create.length, 0);
});
