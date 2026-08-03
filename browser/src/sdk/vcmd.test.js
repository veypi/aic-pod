// vcmd.test.js — page 端虚拟指令测试（node:test，内存 fs mock）。
// 语义对齐 aic-pod/sdk/vcore 的 vectors（ls.json/rg.json/tree.json/fileops.json），
// 目录 size/mtime 为 IndexedDB 模型限制（无目录记录）用 "-"/0 输出。
import { test } from "node:test";
import assert from "node:assert/strict";
import {
  parseVCmdArgv,
  runVCmd,
  cmdLs,
  cmdRg,
  cmdTree,
  cmdRm,
  cmdCurl,
  humanSize,
  globMatch,
  cmpBytes,
} from "./vcmd.js";

// ---- 内存 fs mock（实现 vcmd 适配器接口）----
// files: Map abs -> {content, mtime(ms)}；目录由路径前缀推导。
class MemFS {
  constructor(files) {
    this.files = new Map(); // abs -> {content, mtime}
    this.dirs = new Set(); // 显式目录（key 尾斜杠）
    for (const [p, v] of Object.entries(files)) {
      if (p.endsWith("/")) {
        this.dirs.add(p.replace(/\/$/, ""));
      } else {
        this.files.set(p, typeof v === "string" ? { content: v, mtime: 1700000000000 } : v);
      }
    }
  }
  _abs(p) {
    const s = String(p || "");
    if (s.startsWith("/")) return s;
    if (s.startsWith("$SESSION")) return s.replace("$SESSION", "/sessions/s1");
    if (s.startsWith("$USER")) return s.replace("$USER", "/home/u1");
    return `/sessions/s1/${s}`;
  }
  _childrenOf(prefix) {
    const dirs = new Set();
    for (const k of this.files.keys()) {
      if (!k.startsWith(prefix) || k === prefix) continue;
      const rest = k.slice(prefix.length);
      const seg = rest.split("/")[0];
      dirs.add(seg);
    }
    for (const d of this.dirs) {
      if (d.startsWith(prefix)) {
        const rest = d.slice(prefix.length);
        if (rest) dirs.add(rest.split("/")[0]);
      }
    }
    return [...dirs];
  }
  _sizeOf(rec) {
    return rec.content instanceof Uint8Array ? rec.content.byteLength : byteLen(rec.content);
  }
  async resolve(p, ctx = {}) {
    return this._abs(p);
  }
  async stat(p, ctx = {}) {
    const abs = this._abs(p);
    // 根恒存在（对齐 PageFS.stat：空工作区 ls 得 empty directory）
    if (abs === "/sessions/s1" || abs === "/home/u1") return { path: abs, dir: true, size: 0, mtime: undefined };
    const rec = this.files.get(abs);
    if (rec !== undefined) return { path: abs, dir: false, size: this._sizeOf(rec), mtime: rec.mtime };
    const prefix = abs.endsWith("/") ? abs : abs + "/";
    if (this._childrenOf(prefix).length > 0 || this.dirs.has(abs)) return { path: prefix, dir: true, size: 0, mtime: undefined };
    return null;
  }
  async list(p, ctx = {}) {
    const abs = this._abs(p);
    const prefix = abs.endsWith("/") ? abs : abs + "/";
    const items = [];
    for (const seg of this._childrenOf(prefix)) {
      const full = prefix + seg;
      const rec = this.files.get(full);
      const deeper = this.dirs.has(full) || this._childrenOf(full + "/").length > 0;
      items.push({
        name: seg,
        path: full + (deeper ? "/" : ""),
        dir: deeper,
        size: rec ? this._sizeOf(rec) : undefined,
        mtime: rec ? rec.mtime : undefined,
      });
    }
    return { ok: true, path: prefix, items };
  }
  async walk(p, ctx = {}) {
    const abs = this._abs(p);
    const prefix = abs.endsWith("/") ? abs : abs + "/";
    const items = [];
    for (const [k, rec] of this.files) {
      if (!k.startsWith(prefix)) continue;
      items.push({ path: k, dir: false, size: byteLen(rec.content), mtime: rec.mtime });
    }
    // 推导目录（显式目录 + 深层中间目录）
    const dirs = new Set(this.dirs);
    for (const k of this.files.keys()) {
      if (!k.startsWith(prefix)) continue;
      const rest = k.slice(prefix.length);
      let idx = rest.indexOf("/");
      while (idx >= 0) {
        dirs.add(prefix + rest.slice(0, idx) + "/");
        idx = rest.indexOf("/", idx + 1);
      }
    }
    for (const d of dirs) items.push({ path: d, dir: true, size: 0, mtime: undefined });
    return { ok: true, path: prefix, items };
  }
  async readRaw(p, ctx = {}) {
    const abs = this._abs(p);
    const rec = this.files.get(abs);
    if (rec === undefined) throw new Error("no such file");
    return { content: rec.content, mime: rec.mime || "text/plain", size: this._sizeOf(rec), path: abs };
  }
  async remove(p, ctx = {}) {
    const abs = this._abs(p);
    const rec = this.files.get(abs);
    if (rec !== undefined) {
      this.files.delete(abs);
      return { ok: true, removed: abs };
    }
    const prefix = abs.endsWith("/") ? abs : abs + "/";
    const keys = [...this.files.keys()].filter((k) => k.startsWith(prefix));
    if (keys.length > 0) {
      if (!ctx.recursive) throw new Error(`${abs} is a non-empty directory (use -r)`);
      for (const k of keys) this.files.delete(k);
      return { ok: true, removed: abs, items: keys.length };
    }
    throw new Error(`${abs}: no such file or directory`);
  }
  async writeText(p, text, ctx = {}) {
    const abs = this._abs(p);
    this.files.set(abs, { content: text, mtime: Date.now() });
    return { ok: true, path: abs, size: byteLen(text) };
  }
  async writeBlob(p, blob, ctx = {}) {
    const abs = this._abs(p);
    // 二进制保真：存 Uint8Array + mime（rg 按 mime 跳过非文本）
    const bytes = new Uint8Array(await blob.arrayBuffer());
    this.files.set(abs, { content: bytes, mime: blob.type || "application/octet-stream", mtime: Date.now() });
    return { ok: true, path: abs, size: bytes.byteLength };
  }
}

// 读取内容归一化：MemFS writeBlob 存 Uint8Array，writeText 存 string
function txtOf(c) {
  return c instanceof Uint8Array ? new TextDecoder().decode(c) : String(c);
}

// ---- curl 测试辅助：stub 全局 fetch 返回构造的 Response ----
function stubFetch(resp) {
  const prev = globalThis.fetch;
  globalThis.fetch = async () => resp;
  return () => {
    globalThis.fetch = prev;
  };
}
function byteLen(s) {
  return new TextEncoder().encode(s).length;
}
const CTX = { sessionId: "s1", workdir: "/sessions/s1" };

// ---- parseVCmdArgv ----

test("parse: unknown flag restricted feedback", () => {
  assert.throws(
    () => parseVCmdArgv("ls", { bools: { "-l": true }, minPos: 0, maxPos: 1 }, ["-x"]),
    (e) => e.message === 'ls: flag "-x" is not supported on this environment (restricted)',
  );
});

test("parse: -la combo expands (vcore §5.4)", () => {
  const pa = parseVCmdArgv("ls", { bools: { "-l": true, "-a": true, "-t": true }, minPos: 0, maxPos: 1 }, ["-la", "/d"]);
  assert.equal(pa.bools["-l"], true);
  assert.equal(pa.bools["-a"], true);
  assert.deepEqual(pa.pos, ["/d"]);
});

test("parse: combo with unknown flag restricted", () => {
  assert.throws(
    () => parseVCmdArgv("ls", { bools: { "-l": true }, minPos: 0, maxPos: 1 }, ["-lx"]),
    (e) => e.message === 'ls: flag "-lx" is not supported on this environment (restricted)',
  );
});

test("parse: equals form restricted feedback", () => {
  assert.throws(
    () => parseVCmdArgv("ls", { bools: { "-l": true }, minPos: 0, maxPos: 1 }, ["--la=true"]),
    (e) => e.message.includes('is not supported on this environment (restricted; use "--flag value" two-element form)'),
  );
});

test("parse: value flag consumes next element", () => {
  const pa = parseVCmdArgv("tree", { values: { "-L": true }, minPos: 0, maxPos: 1 }, ["-L", "2"]);
  assert.equal(pa.values["-L"], "2");
});

test("parse: value flag requires value", () => {
  assert.throws(
    () => parseVCmdArgv("tree", { values: { "-L": true }, minPos: 0, maxPos: 1 }, ["-L"]),
    (e) => e.message === 'tree: flag "-L" requires a value',
  );
});

test("parse: extra positional", () => {
  assert.throws(
    () => parseVCmdArgv("ls", { minPos: 0, maxPos: 1 }, ["/a", "/b"]),
    (e) => e.message === 'ls: unexpected argument "/b"',
  );
});

test("parse: missing positional", () => {
  assert.throws(
    () => parseVCmdArgv("rm", { bools: { "-r": true }, minPos: 1, maxPos: 1 }, []),
    (e) => e.message === "rm: missing argument (expected at least 1, got 0)",
  );
});

// ---- humanSize / globMatch / cmpBytes ----

test("humanSize: 1024-based suffixes (GNU ls -h aligned)", () => {
  assert.equal(humanSize(0), "0B");
  assert.equal(humanSize(36), "36B");
  assert.equal(humanSize(1024), "1.0K");
  assert.equal(humanSize(1536), "1.5K");
  assert.equal(humanSize(1048576), "1.0M");
});

test("globMatch: * and ? full match", () => {
  assert.equal(globMatch("*.go", "a.go"), true);
  assert.equal(globMatch("*.go", "a.txt"), false);
  assert.equal(globMatch("a?c", "abc"), true);
  assert.equal(globMatch("a?c", "ac"), false);
  assert.equal(globMatch("*", ".hidden"), true);
});

test("cmpBytes: UTF-8 byte order", () => {
  assert.ok(cmpBytes("a.txt", "b.txt") < 0);
  assert.ok(cmpBytes("b", "a") > 0);
  assert.equal(cmpBytes("x", "x"), 0);
  assert.ok(cmpBytes("ab", "a") > 0);
});

// ---- ls（对齐 vectors/ls.json）----

test("ls: hides dotfiles by default", async () => {
  const fs = new MemFS({ "/docs/a.txt": "a", "/docs/.env": "x", "/docs/.git/": "" });
  const r = await cmdLs(fs, CTX, ["/docs"]);
  assert.equal(r.content, "a.txt");
  assert.deepEqual(r.attrs, { action: "ls", path: "/docs", path_kind: "directory", rows: "1", truncated: "false" });
});

test("ls: -a shows dotfiles", async () => {
  const fs = new MemFS({ "/docs/a.txt": "a", "/docs/.env": "x", "/docs/.git/": "" });
  const r = await cmdLs(fs, CTX, ["-a", "/docs"]);
  assert.equal(r.content, ".env\n.git/\na.txt");
  assert.equal(r.attrs.rows, "3");
});

test("ls: -la combo size and mtime columns", async () => {
  const fs = new MemFS({
    "/docs/b.txt": { content: "bb", mtime: 1700000001000 },
    "/docs/a.txt": { content: "a", mtime: 1700000000000 },
    "/docs/.env": { content: "xyz", mtime: 1700000002000 },
  });
  const r = await cmdLs(fs, CTX, ["-la", "/docs"]);
  assert.equal(r.content, ".env\t3\t1700000002\na.txt\t1\t1700000000\nb.txt\t2\t1700000001");
});

test("ls: -lh human readable size", async () => {
  const fs = new MemFS({ "/docs/big.bin": { content: "a".repeat(1024), mtime: 1700000000000 } });
  const r = await cmdLs(fs, CTX, ["-lh", "/docs"]);
  assert.equal(r.content, "big.bin\t1.0K\t1700000000");
});

test("ls: -t sorts by mtime desc, ties by name", async () => {
  const fs = new MemFS({
    "/docs/b.txt": { content: "b", mtime: 1700000000000 },
    "/docs/a.txt": { content: "a", mtime: 1700000000000 },
    "/docs/c.txt": { content: "c", mtime: 1700000000000 },
  });
  const r = await cmdLs(fs, CTX, ["-t", "/docs"]);
  assert.equal(r.content, "a.txt\nb.txt\nc.txt");
});

test("ls: single file prints name", async () => {
  const fs = new MemFS({ "/docs/a.txt": "a" });
  const r = await cmdLs(fs, CTX, ["/docs/a.txt"]);
  assert.equal(r.content, "a.txt");
  assert.equal(r.attrs.path_kind, "file");
});

test("ls: empty directory (root always exists)", async () => {
  const fs = new MemFS({});
  const r = await cmdLs(fs, CTX, []); // workdir = /sessions/s1 根
  assert.equal(r.content, "empty directory: /sessions/s1");
  assert.equal(r.attrs.rows, "0");
});

test("ls: missing path error", async () => {
  const fs = new MemFS({});
  await assert.rejects(cmdLs(fs, CTX, ["/nope"]), (e) => e.message === "ls: /nope: no such file or directory");
});

// ---- rg（对齐 vectors/rg.json）----

test("rg: default search path:line:content", async () => {
  const fs = new MemFS({
    "/d/a.txt": "hello world\nfoo bar\nhello again",
    "/d/b.txt": "nothing here",
    "/d/.hidden.txt": "hello secret",
  });
  const r = await cmdRg(fs, CTX, ["hello", "/d"]);
  assert.equal(r.content, "/d/a.txt:1:hello world\n/d/a.txt:3:hello again");
  assert.equal(r.attrs.rows, "2");
});

test("rg: -c counts per file", async () => {
  const fs = new MemFS({ "/d/a.txt": "hello\nx\nhello" });
  const r = await cmdRg(fs, CTX, ["-c", "hello", "/d"]);
  assert.equal(r.content, "/d/a.txt:2");
});

test("rg: -w whole word", async () => {
  const fs = new MemFS({ "/d/a.txt": "hello world\nshell" });
  const r = await cmdRg(fs, CTX, ["-w", "hell", "/d"]);
  assert.equal(r.content, "no matches for pattern \"hell\" in /d");
});

test("rg: -i case insensitive", async () => {
  const fs = new MemFS({ "/d/a.txt": "HELLO" });
  const r = await cmdRg(fs, CTX, ["-i", "hello", "/d"]);
  assert.equal(r.content, "/d/a.txt:1:HELLO");
});

test("rg: --files lists files, skips hidden", async () => {
  const fs = new MemFS({ "/d/a.txt": "a", "/d/.env": "x", "/d/sub/b.txt": "b" });
  const r = await cmdRg(fs, CTX, ["--files", "/d"]);
  assert.equal(r.content, "/d/a.txt\n/d/sub/b.txt");
});

test("rg: --hidden includes hidden", async () => {
  const fs = new MemFS({ "/d/a.txt": "a", "/d/.env": "x" });
  const r = await cmdRg(fs, CTX, ["--files", "--hidden", "/d"]);
  assert.equal(r.content, "/d/.env\n/d/a.txt");
});

test("rg: -g glob filters files", async () => {
  const fs = new MemFS({ "/d/a.go": "a", "/d/b.txt": "b" });
  const r = await cmdRg(fs, CTX, ["--files", "-g", "*.go", "/d"]);
  assert.equal(r.content, "/d/a.go");
});

test("rg: glob with ! or ** restricted", async () => {
  await assert.rejects(cmdRg(new MemFS({}), CTX, ["--files", "-g", "!x", "/d"]), (e) =>
    e.message.includes('glob "!x" is not supported on this environment (restricted: no \'!\' negation or \'**\')'),
  );
});

test("rg: --files with search flags rejected", async () => {
  await assert.rejects(cmdRg(new MemFS({}), CTX, ["--files", "-c", "/d"]), (e) =>
    e.message === "rg: --files cannot be used with search flags (-i, -l, -m, -n, -c, -w)",
  );
});

test("rg: lookaround pattern restricted with shell hint", async () => {
  await assert.rejects(cmdRg(new MemFS({}), CTX, ["(?=x)y", "/d"]), (e) =>
    e.message.includes('pattern is not supported on this environment (restricted: no lookaround/backreference), use bash -c "grep -P ..." on a physical host'),
  );
});

test("rg: backreference pattern restricted", async () => {
  await assert.rejects(cmdRg(new MemFS({}), CTX, ["(a)\\1", "/d"]), (e) =>
    e.message.includes("no lookaround/backreference"),
  );
});

test("rg: no matches message", async () => {
  const fs = new MemFS({ "/d/a.txt": "x" });
  const r = await cmdRg(fs, CTX, ["zzz", "/d"]);
  assert.equal(r.content, "no matches for pattern \"zzz\" in /d");
});

test("rg: single file explicit path", async () => {
  const fs = new MemFS({ "/d/a.txt": "needle here" });
  const r = await cmdRg(fs, CTX, ["needle", "/d/a.txt"]);
  assert.equal(r.content, "/d/a.txt:1:needle here");
});

test("rg: skips node_modules and vendor dirs", async () => {
  const fs = new MemFS({ "/d/a.txt": "x", "/d/node_modules/pkg/b.txt": "needle" });
  const r = await cmdRg(fs, CTX, ["needle", "/d"]);
  assert.equal(r.content, 'no matches for pattern "needle" in /d');
});

test("rg: CRLF stripped", async () => {
  const fs = new MemFS({ "/d/a.txt": "hello\r\nworld\r\n" });
  const r = await cmdRg(fs, CTX, ["hello", "/d"]);
  assert.equal(r.content, "/d/a.txt:1:hello");
});

// ---- tree（对齐 vectors/tree.json + 隐藏项新语义）----

test("tree: flat JSON output", async () => {
  const fs = new MemFS({ "/d/a.txt": "a", "/d/b.txt": "b", "/d/sub/": "" });
  const r = await cmdTree(fs, CTX, ["/d"]);
  const parsed = JSON.parse(r.content);
  assert.equal(parsed.cwd, "/d");
  assert.equal(parsed.dir, true);
  assert.deepEqual(
    parsed.items.map((i) => i.name),
    ["a.txt", "b.txt", "sub"],
  );
  assert.equal(r.attrs.rows, "3");
});

test("tree: -L depth expands nested", async () => {
  const fs = new MemFS({ "/d/a/b/c.txt": "x" });
  const r = await cmdTree(fs, CTX, ["-L", "3", "/d"]);
  const parsed = JSON.parse(r.content);
  assert.equal(parsed.items[0].name, "a");
  assert.equal(parsed.items[0].items[0].name, "b");
  assert.equal(parsed.items[0].items[0].items[0].name, "c.txt");
});

test("tree: hidden items excluded entirely", async () => {
  const fs = new MemFS({ "/d/ok.txt": "o", "/d/.env": "e", "/d/.git/y.txt": "y" });
  const r = await cmdTree(fs, CTX, ["--depth", "2", "/d"]);
  const parsed = JSON.parse(r.content);
  assert.deepEqual(parsed.items.map((i) => i.name), ["ok.txt"]);
  assert.equal(r.attrs.rows, "1");
});

test("tree: node_modules kept as leaf without recursion", async () => {
  const fs = new MemFS({ "/d/ok.txt": "o", "/d/node_modules/x.txt": "x" });
  const r = await cmdTree(fs, CTX, ["--depth", "2", "/d"]);
  const parsed = JSON.parse(r.content);
  const nm = parsed.items.find((i) => i.name === "node_modules");
  assert.ok(nm);
  assert.equal(nm.items, undefined); // 不展开
  assert.equal(nm.dir, true);
});

test("tree: single file entry", async () => {
  const fs = new MemFS({ "/d/a.txt": "a" });
  const r = await cmdTree(fs, CTX, ["/d/a.txt"]);
  assert.equal(r.content, '{"name":"a.txt","dir":false,"size":1,"mod_time":1700000000}');
});

test("tree: invalid depth", async () => {
  await assert.rejects(cmdTree(new MemFS({}), CTX, ["--depth", "0", "/d"]), (e) =>
    e.message === "tree: --depth must be >= 1, got 0",
  );
});

test("tree: default target = session root (no path, hfs-style --depth only)", async () => {
  const fs = new MemFS({ "/sessions/s1/a.txt": "a", "/sessions/s1/sub/b.txt": "b" });
  const r = await cmdTree(fs, CTX, ["--depth", "3"]);
  const parsed = JSON.parse(r.content);
  assert.equal(parsed.cwd, "/sessions/s1");
  assert.deepEqual(parsed.items.map((i) => i.name), ["a.txt", "sub"]);
});

test("ls: default target = session root (no path)", async () => {
  const fs = new MemFS({ "/sessions/s1/a.txt": "a" });
  const r = await cmdLs(fs, CTX, []);
  assert.equal(r.content, "a.txt");
  assert.equal(r.attrs.path, "/sessions/s1");
});

test("tree: unknown flag restricted", async () => {
  await assert.rejects(cmdTree(new MemFS({}), CTX, ["-x", "/d"]), (e) =>
    e.message === 'tree: flag "-x" is not supported on this environment (restricted)',
  );
});

// ---- rm（对齐 fileops 语义）----

test("rm: file removed", async () => {
  const fs = new MemFS({ "/d/a.txt": "a" });
  const r = await cmdRm(fs, CTX, ["/d/a.txt"]);
  assert.equal(r.content, "removed /d/a.txt");
});

test("rm: non-empty dir requires -r", async () => {
  const fs = new MemFS({ "/d/a.txt": "a" });
  await assert.rejects(cmdRm(fs, CTX, ["/d"]), (e) => e.message === "rm: /d is a non-empty directory (use -r)");
});

test("rm: -r recursive with items count", async () => {
  const fs = new MemFS({ "/d/a.txt": "a", "/d/sub/b.txt": "b" });
  const r = await cmdRm(fs, CTX, ["-r", "/d"]);
  assert.equal(r.content, "removed /d (2 items)");
});

test("rm: missing path", async () => {
  const fs = new MemFS({});
  await assert.rejects(cmdRm(fs, CTX, ["/nope"]), (e) => e.message === "rm: /nope: no such file or directory");
});

// ---- curl（流式下载：边读边计数、超限中止不落盘、Blob 二进制保留）----

test("curl: streams text and writes blob", async () => {
  const fs = new MemFS({});
  const restore = stubFetch(new Response("hello world", { status: 200 }));
  try {
    const r = await cmdCurl(fs, CTX, ["-o", "/d/f.txt", "https://example.com/f.txt"]);
    assert.equal(r.content, "downloaded https://example.com/f.txt to /d/f.txt (11 bytes)");
    assert.deepEqual(r.attrs, { action: "curl", path: "/d/f.txt", bytes: "11" });
    const rec = fs.files.get("/d/f.txt");
    assert.equal(new TextDecoder().decode(rec.content), "hello world");
  } finally {
    restore();
  }
});

test("curl: binary bytes preserved (no mojibake)", async () => {
  const fs = new MemFS({});
  const bytes = new Uint8Array([0x00, 0xff, 0x10, 0x7f, 0x80, 0x9f]);
  const restore = stubFetch(new Response(bytes, { status: 200 }));
  try {
    const r = await cmdCurl(fs, CTX, ["-o", "/d/bin.dat", "https://example.com/bin.dat"]);
    assert.equal(r.attrs.bytes, String(bytes.byteLength));
    const rec = fs.files.get("/d/bin.dat");
    assert.ok(rec.content instanceof Uint8Array);
    assert.deepEqual([...rec.content], [...bytes]);
  } finally {
    restore();
  }
});

test("curl: size limit aborts stream without writing", async () => {
  const fs = new MemFS({});
  // 2MB 响应 vs --max-size 1：流式读取中计数超限即中止，不落盘
  const restore = stubFetch(new Response(new Uint8Array(2 * 1024 * 1024), { status: 200 }));
  try {
    await assert.rejects(cmdCurl(fs, CTX, ["-o", "/d/big.bin", "--max-size", "1", "https://example.com/big.bin"]), (e) =>
      e.message.startsWith("curl: size limit exceeded (2MB > 1MB)"),
    );
    assert.equal(fs.files.has("/d/big.bin"), false); // 无半成品
  } finally {
    restore();
  }
});

test("curl: destination exists rejected", async () => {
  const fs = new MemFS({ "/d/f.txt": "old" });
  const restore = stubFetch(new Response("new", { status: 200 }));
  try {
    await assert.rejects(cmdCurl(fs, CTX, ["-o", "/d/f.txt", "https://example.com/f.txt"]), (e) =>
      e.message === "curl: destination /d/f.txt already exists",
    );
    assert.equal(fs.files.get("/d/f.txt").content, "old"); // 未覆盖
  } finally {
    restore();
  }
});

test("curl: -o required and scheme rejected", async () => {
  const fs = new MemFS({});
  await assert.rejects(cmdCurl(fs, CTX, ["https://example.com/x"]), (e) => e.message === "curl: -o is required");
  await assert.rejects(cmdCurl(fs, CTX, ["-o", "/d/x", "ftp://example.com/x"]), (e) =>
    e.message === "curl: scheme not yet supported: ftp",
  );
  await assert.rejects(cmdCurl(fs, CTX, ["-o", "/d/x", "example.com/x"]), (e) =>
    e.message === 'curl: invalid url "example.com/x": missing scheme',
  );
});

test("curl: http error status", async () => {
  const fs = new MemFS({});
  const restore = stubFetch(new Response("nf", { status: 404 }));
  try {
    await assert.rejects(cmdCurl(fs, CTX, ["-o", "/d/x", "https://example.com/x"]), (e) =>
      e.message === "curl: fetch https://example.com/x: HTTP 404",
    );
    assert.equal(fs.files.has("/d/x"), false);
  } finally {
    restore();
  }
});

// ---- runVCmd 入口 ----

test("runVCmd: unknown builtin", async () => {
  await assert.rejects(runVCmd("chmod", [], new MemFS({}), CTX), (e) =>
    e.message.includes("unknown builtin (supported: ls, rg, tree, rm, curl, cp, mv)"),
  );
});

test("runVCmd: ls through entry", async () => {
  const fs = new MemFS({ "/d/a.txt": "a" });
  const r = await runVCmd("ls", ["/d"], fs, CTX);
  assert.equal(r.content, "a.txt");
});

// ---- cp / mv ----

test("mv: 文件重命名", async () => {
  const fs = new MemFS({ "/d/a.txt": "hello" });
  const r = await runVCmd("mv", ["/d/a.txt", "/d/b.txt"], fs, CTX);
  assert.match(r.content, /^moved \/d\/a\.txt to \/d\/b\.txt$/);
  assert.equal(r.attrs.action, "mv");
  assert.equal(r.attrs.source_path, "/d/a.txt");
  assert.equal(txtOf((await fs.readRaw("/d/b.txt", CTX)).content), "hello");
  await assert.rejects(fs.readRaw("/d/a.txt", CTX), /no such file/);
});

test("mv: 目录整体移动（含子文件）", async () => {
  // 二进制文件须按 {content, mtime} 包装（裸 Uint8Array 时 MemFS.readRaw 取不到 content）
  const fs = new MemFS({ "/p/x/a.txt": "1", "/p/x/sub/b.bin": { content: new Uint8Array([1, 2, 3]), mtime: 1700000000000 } });
  await runVCmd("mv", ["/p/x", "/p/y"], fs, CTX);
  assert.equal(txtOf((await fs.readRaw("/p/y/a.txt", CTX)).content), "1");
  const bin = await fs.readRaw("/p/y/sub/b.bin", CTX);
  assert.ok(bin.content instanceof Uint8Array && bin.content.byteLength === 3);
  await assert.rejects(fs.readRaw("/p/x/a.txt", CTX), /no such file/);
});

test("mv: 目标已存在报错不覆盖", async () => {
  const fs = new MemFS({ "/d/a.txt": "1", "/d/b.txt": "2" });
  await assert.rejects(runVCmd("mv", ["/d/a.txt", "/d/b.txt"], fs, CTX), /destination \/d\/b\.txt already exists/);
  assert.equal(txtOf((await fs.readRaw("/d/b.txt", CTX)).content), "2");
});

test("mv: 目录移入自身子路径报错", async () => {
  const fs = new MemFS({ "/p/x/a.txt": "1" });
  await assert.rejects(runVCmd("mv", ["/p/x", "/p/x/sub"], fs, CTX), /cannot move directory \/p\/x into itself/);
});

test("mv: 源不存在报错", async () => {
  const fs = new MemFS({});
  await assert.rejects(runVCmd("mv", ["/d/nope", "/d/x"], fs, CTX), /cannot stat source \/d\/nope: no such file or directory/);
});

test("cp: 文件复制（保留源）", async () => {
  const fs = new MemFS({ "/d/a.txt": "hello" });
  const r = await runVCmd("cp", ["/d/a.txt", "/d/c.txt"], fs, CTX);
  assert.match(r.content, /^copied \/d\/a\.txt to \/d\/c\.txt$/);
  assert.equal(txtOf((await fs.readRaw("/d/a.txt", CTX)).content), "hello");
  assert.equal(txtOf((await fs.readRaw("/d/c.txt", CTX)).content), "hello");
});

test("cp: 目录需 -r", async () => {
  const fs = new MemFS({ "/p/x/a.txt": "1" });
  await assert.rejects(runVCmd("cp", ["/p/x", "/p/y"], fs, CTX), /is a directory \(use -r\)/);
  const r = await runVCmd("cp", ["-r", "/p/x", "/p/y"], fs, CTX);
  assert.match(r.content, /^copied \/p\/x to \/p\/y$/);
  assert.equal(txtOf((await fs.readRaw("/p/y/a.txt", CTX)).content), "1");
  assert.equal((await fs.readRaw("/p/x/a.txt", CTX)).content, "1");
});
