// fsops.test.js — fs 指令集（ls/rg/cp/mv/rm）JS 实现测试（node:test，内存 fs mock）。
// 与 aic-pod/libs/vcore/testdata/vectors 同向量运行（§2.6 三端一致门禁）：
// ls.json / rg.json / fileops.json 的 fs 用例在 MemFS 替身上执行，
// Content/Attrs/错误消息必须与 Go 逐字节一致（目录 size/mtime 为模型限制归一为 0）。
import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync, readdirSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { join, dirname } from "node:path";
import { runFsOps, globMatch, cmpBytes } from "./fsops.js";

const VECTORS_DIR = join(
  dirname(fileURLToPath(import.meta.url)),
  "../../../libs/vcore/testdata/vectors",
);

// ---- 内存 fs mock（实现 fsops 适配器接口；ctx.workdir 感知）----
class MemFS {
  constructor(files, mtimes) {
    this.files = new Map(); // abs -> {content: string|Uint8Array, mtime(ms), mime}
    this.dirs = new Set(); // 显式目录（无尾斜杠）
    for (const [p, v] of Object.entries(files || {})) {
      if (p.endsWith("/")) {
        this.dirs.add(p.replace(/\/$/, ""));
        continue;
      }
      const mt = ((mtimes || {})[p] ?? 0) * 1000;
      if (typeof v === "string" && v.startsWith("base64:")) {
        this.files.set(p, { content: Buffer.from(v.slice(7), "base64"), mtime: mt, mime: "application/octet-stream" });
      } else {
        this.files.set(p, { content: String(v), mtime: mt, mime: "text/plain" });
      }
    }
  }
  _clean(p) {
    const parts = [];
    for (const seg of p.split("/")) {
      if (!seg || seg === ".") continue;
      if (seg === "..") parts.pop();
      else parts.push(seg);
    }
    return "/" + parts.join("/");
  }
  _path(p, ctx = {}) {
    let s = String(p || "");
    if (!s.startsWith("/")) s = (ctx.workdir || "/") + "/" + s;
    return this._clean(s);
  }
  _sizeOf(rec) {
    return rec.content instanceof Uint8Array ? rec.content.byteLength : Buffer.byteLength(rec.content);
  }
  _childrenOf(prefix) {
    const segs = new Set();
    const collect = (k) => {
      if (!k.startsWith(prefix) || k === prefix) return;
      const rest = k.slice(prefix.length);
      if (rest) segs.add(rest.split("/")[0]);
    };
    for (const k of this.files.keys()) collect(k);
    for (const d of this.dirs) collect(d + "/");
    return [...segs];
  }
  async stat(p, ctx = {}) {
    const abs = this._path(p, ctx);
    if (abs === "/") return { path: "/", dir: true, size: 0, mtime: undefined };
    const rec = this.files.get(abs);
    if (rec !== undefined) return { path: abs, dir: false, size: this._sizeOf(rec), mtime: rec.mtime };
    const prefix = abs + "/";
    if (this.dirs.has(abs) || this._childrenOf(prefix).length > 0) {
      return { path: prefix, dir: true, size: 0, mtime: undefined };
    }
    return null;
  }
  async list(p, ctx = {}) {
    const abs = this._path(p, ctx);
    const prefix = abs.endsWith("/") ? abs : abs + "/";
    const items = [];
    for (const seg of this._childrenOf(prefix)) {
      const full = prefix + seg;
      const rec = this.files.get(full);
      const deeper = !rec && (this.dirs.has(full) || this._childrenOf(full + "/").length > 0);
      items.push({
        name: seg,
        path: full + (deeper ? "/" : ""),
        dir: deeper,
        size: rec ? this._sizeOf(rec) : undefined,
        mtime: rec ? rec.mtime : undefined,
      });
    }
    items.sort((a, b) => cmpBytes(a.name, b.name));
    return { ok: true, path: prefix, items };
  }
  async walk(p, ctx = {}) {
    const abs = this._path(p, ctx);
    const prefix = abs.endsWith("/") ? abs : abs + "/";
    const items = [];
    for (const [k, rec] of this.files) {
      if (!k.startsWith(prefix)) continue;
      items.push({ path: k, dir: false, size: this._sizeOf(rec), mtime: rec.mtime });
    }
    const dirs = new Set(this.dirs);
    for (const k of this.files.keys()) {
      if (!k.startsWith(prefix)) continue;
      const rest = k.slice(prefix.length);
      let idx = rest.indexOf("/");
      while (idx >= 0) {
        dirs.add((prefix + rest.slice(0, idx)).replace(/\/$/, ""));
        idx = rest.indexOf("/", idx + 1);
      }
    }
    for (const d of dirs) {
      if (d.startsWith(prefix) || d + "/" === prefix) items.push({ path: d + "/", dir: true, size: 0, mtime: undefined });
    }
    return { ok: true, path: prefix, items };
  }
  async readRaw(p, ctx = {}) {
    const abs = this._path(p, ctx);
    const rec = this.files.get(abs);
    if (rec === undefined) throw new Error(`fs read: ${abs}: no such file or directory`);
    return { content: rec.content, mime: rec.mime, size: this._sizeOf(rec), path: abs };
  }
  async writeBlob(p, blob, ctx = {}) {
    const abs = this._path(p, ctx);
    const buf = new Uint8Array(await blob.arrayBuffer());
    const text = blob.type === "text/plain" || !blob.type ? new TextDecoder().decode(buf) : buf;
    this.files.set(abs, {
      content: text,
      mtime: Date.now(),
      mime: blob.type || "text/plain",
    });
    return { ok: true, path: abs, size: buf.byteLength };
  }
  async remove(p, ctx = {}) {
    const abs = this._path(p, ctx);
    if (abs === "/") throw new Error("fs rm: cannot remove root");
    const rec = this.files.get(abs);
    if (rec !== undefined) {
      this.files.delete(abs);
      return { ok: true, removed: abs };
    }
    const prefix = abs + "/";
    const fileKeys = [...this.files.keys()].filter((k) => k.startsWith(prefix));
    // 目录条目 = 显式目录 + 文件路径推导的中间目录（对齐 vcore countEntries 计数）
    const dirKeys = new Set([...this.dirs].filter((d) => d !== abs && (d + "/").startsWith(prefix)));
    for (const k of fileKeys) {
      let idx = k.indexOf("/", prefix.length);
      while (idx >= 0) {
        dirKeys.add(k.slice(0, idx));
        idx = k.indexOf("/", idx + 1);
      }
    }
    if (fileKeys.length + dirKeys.size > 0) {
      if (!ctx.recursive) throw new Error(`fs rm: ${abs} is a non-empty directory (set recursive=true)`);
      for (const k of fileKeys) this.files.delete(k);
      for (const d of dirKeys) this.dirs.delete(d);
      return { ok: true, removed: abs, items: fileKeys.length + dirKeys.size };
    }
    if (this.dirs.delete(abs)) return { ok: true, removed: abs };
    throw new Error(`fs rm: ${abs}: no such file or directory`);
  }
  dump() {
    const out = {};
    for (const d of this.dirs) out[d + "/"] = "";
    for (const [k, rec] of this.files) {
      out[k] = rec.content instanceof Uint8Array ? new TextDecoder().decode(rec.content) : rec.content;
    }
    // 推导中间目录
    for (const k of [...this.files.keys(), ...this.dirs]) {
      let idx = k.indexOf("/", 1);
      while (idx >= 0) {
        out[k.slice(0, idx) + "/"] = "";
        idx = k.indexOf("/", idx + 1);
      }
    }
    return out;
  }
}

// ---- 向量运行器 ----

// JS 端不覆盖的维度（Go 端已锁）：vars / protectRoots / fetch（exec curl）。
function skipCase(c) {
  if (c.vars || c.protectRoots || c.fetch) return true;
  // 未知字段拒绝是信封层（PageFS.run ALLOWED_FIELDS）职责，fsops 层不重复
  if (c.name && c.name.includes("unknown field")) return true;
  return false;
}

for (const file of readdirSync(VECTORS_DIR)) {
  const vf = JSON.parse(readFileSync(join(VECTORS_DIR, file), "utf8"));
  for (const c of vf.cases || []) {
    if (c.cmd !== "fs") continue;
    const action = c.params?.action;
    if (!["ls", "rg", "cp", "mv", "rm"].includes(action)) continue; // read/write/edit 走 PageFS 本体
    if (skipCase(c)) continue;
    test(`${file}/${c.name}`, async () => {
      const fs = new MemFS(c.files, c.mtimes);
      const ctx = { workdir: c.workdir || "/" };
      let res, err;
      try {
        res = await runFsOps(fs, c.params, ctx);
      } catch (e) {
        err = e;
      }
      if (c.expectError) {
        assert.ok(err, `want error ${c.expectError}, got result ${JSON.stringify(res)}`);
        // 正则引擎错误文案 Go/JS 各自不同（RE2 vs V8），只比对前缀
        if (c.expectError.includes("invalid pattern")) {
          assert.ok(
            err.message.startsWith("fs rg: invalid pattern:"),
            `error = ${err.message}`,
          );
        } else {
          assert.equal(err.message, c.expectError);
        }
        return;
      }
      assert.ifError(err);
      if (c.expect) {
        assert.equal(res.content, c.expect.content);
        assert.deepEqual(res.attrs, c.expect.attrs);
      }
      if (c.expectFiles) {
        assert.deepEqual(fs.dump(), c.expectFiles);
      }
    });
  }
}

// ---- 单元补充 ----

test("globMatch: * and ?", () => {
  assert.ok(globMatch("*.go", "a.go"));
  assert.ok(!globMatch("*.go", "a.gox"));
  assert.ok(globMatch("a?.go", "ab.go"));
});

test("cmpBytes: UTF-8 byte order", () => {
  assert.ok(cmpBytes("a", "b") < 0);
  assert.ok(cmpBytes("abc", "abd") < 0);
  assert.equal(cmpBytes("x", "x"), 0);
});
