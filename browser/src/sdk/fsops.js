// fsops.js — fs 指令集的 ls/rg/cp/mv/rm 实现（JSON 参数，PageFS 原语驱动）。
//
// 双端复用（同一套代码逻辑，逐字节同步，禁止漂移）：
//   - aic/ui/assets/libs/fsops.js   — page 端（1host="page"，页面 IndexedDB）
//   - aic-pod/browser/src/sdk/fsops.js — 浏览器扩展端（1host=host_id，扩展 IndexedDB）
//
// 语义对齐 aic-pod/libs/vcore（§2.6 三端一致）：输出/attrs/错误文案与 Go 一致
// （错误前缀 "fs {action}: ..."）。目录 size/mtime 为 IndexedDB 模型限制
// （无目录记录）输出 0（Go UFS 端为真实数值；一致性向量运行器归一处理）。
//
// 适配器接口（PageFS / 测试 MemFS 均满足）：
//   stat(p) → {path,dir,size?,mtime?} | null   list(p) → {items:[{name,path,dir,size?,mtime?}]}
//   walk(p) → {items:[{path,dir,size?,mtime?}]}  readRaw(p) → {content,mime,size,path}
//   remove(p, {recursive}) → {removed, items?}   writeBlob(p, blob) → {path, size}
//   _path?(p) → abs（可选，缺省原样）
// ctx: {workdir?}（缺省目标 = ctx.workdir > "/"）。

// fsErr 与 vcore fsErr 文案一致：fs {action}: {原因}。
function fsErr(action, msg) {
  return new Error(action ? `fs ${action}: ${msg}` : `fs: ${msg}`);
}

// cmpBytes 按 UTF-8 字节序比较（禁止 locale 相关排序，§5.4）。
export function cmpBytes(a, b) {
  const ae = new TextEncoder().encode(a);
  const be = new TextEncoder().encode(b);
  const n = Math.min(ae.length, be.length);
  for (let i = 0; i < n; i++) {
    if (ae[i] !== be[i]) return ae[i] - be[i];
  }
  return ae.length - be.length;
}

// globMatch：rg glob 文件名匹配（* 任意序列 / ? 单字符，完整匹配，线性双指针）。
export function globMatch(pattern, s) {
  let p = 0;
  let star = -1;
  let mark = 0;
  for (let i = 0; i < s.length; ) {
    if (p < pattern.length && (pattern[p] === "?" || pattern[p] === s[i])) {
      p++;
      i++;
    } else if (p < pattern.length && pattern[p] === "*") {
      star = p;
      mark = i;
      p++;
    } else if (star >= 0) {
      mark++;
      i = mark;
      p = star + 1;
    } else {
      return false;
    }
  }
  while (p < pattern.length && pattern[p] === "*") p++;
  return p === pattern.length;
}

// LS_SKIP_DIRS 是 ls 递归不 descend 的目录（与 vcore lsSkipDirs 一致，叶条目保留）。
const LS_SKIP_DIRS = new Set([
  "node_modules", "vendor", "__pycache__", "bower_components", "dist", "build",
  "target", ".next", ".nuxt", "coverage", ".turbo", ".output",
]);
// RG_SKIP_DIRS 是 rg 恒跳过集（与 vcore skipDirs 一致）。
const RG_SKIP_DIRS = new Set(["node_modules", "vendor"]);

const LS_DEFAULT_DEPTH = 1;
const LS_MAX_DEPTH = 5;
const LS_MAX_NODES = 2000;
const RG_DEFAULT_LIMIT = 100;
// RG_MAX_LINE_BYTES 匹配行内容单行字节上限 / RG_MAX_CONTENT_BYTES 总预算
// （对齐 vcore rg.go §2.5：minified 单行超长命中不撑爆上下文）。
const RG_MAX_LINE_BYTES = 8 << 10;
const RG_MAX_CONTENT_BYTES = 512 << 10;

function isHidden(name) {
  return name.startsWith(".");
}

// defaultTarget 缺省目标路径：显式 path > ctx.workdir > 本地根 /（本地空间无会话概念）。
function defaultTarget(ctx) {
  return ctx?.workdir || "/";
}

async function absOf(fs, target, ctx) {
  return fs._path ? await fs._path(target, ctx) : target;
}

// ---- ls（对齐 vcore ls.go：JSON 树输出，depth>1 递归）----

async function fsLs(fs, ctx, p) {
  let depth = LS_DEFAULT_DEPTH;
  if (p.depth !== undefined && p.depth !== null) {
    depth = Number(p.depth);
    if (!Number.isInteger(depth) || depth < 1) throw fsErr("ls", `depth must be >= 1, got ${p.depth}`);
  }
  if (depth > LS_MAX_DEPTH) depth = LS_MAX_DEPTH;
  const byTime = p.sort === "time";
  if (p.sort !== undefined && p.sort !== "" && p.sort !== "name" && p.sort !== "time") {
    throw fsErr("ls", `sort must be "name" or "time", got "${p.sort}"`);
  }
  const all = !!p.all;

  const target = p.path || defaultTarget(ctx);
  const abs = await absOf(fs, target, ctx);
  const st = await fs.stat(target, ctx);
  if (st === null) throw fsErr("ls", `${abs}: no such file or directory`);

  if (!st.dir) {
    const name = st.path.split("/").filter(Boolean).pop() || "";
    const entry = { name, dir: false, size: st.size ?? 0, mod_time: st.mtime === undefined ? 0 : Math.floor(st.mtime / 1000) };
    return { content: JSON.stringify(entry), attrs: { action: "ls", path: abs, rows: "1", truncated: "false" } };
  }

  const state = { count: 0, truncated: false };
  const items = await buildLsDir(fs, ctx, target, depth, all, state);
  sortLsEntries(items, byTime);
  const out = { cwd: abs, dir: true, items, truncated: state.truncated };
  return { content: JSON.stringify(out), attrs: { action: "ls", path: abs, rows: String(state.count), truncated: String(state.truncated) } };
}

async function buildLsDir(fs, ctx, dir, remain, all, state) {
  if (state.truncated) return [];
  const res = await fs.list(dir, ctx);
  const out = [];
  for (const e of res.items || []) {
    if (state.count >= LS_MAX_NODES) {
      state.truncated = true;
      break;
    }
    const name = e.name;
    // 隐藏项（点开头）默认完全跳过：不显示不递归；all=true 收录
    if (!all && isHidden(name)) continue;
    const size = e.dir ? 0 : (e.size ?? 0);
    const mt = e.mtime === undefined ? 0 : Math.floor(e.mtime / 1000);
    const ent = { name, dir: e.dir, size, mod_time: mt };
    state.count++;
    if (e.dir && remain > 1 && !LS_SKIP_DIRS.has(name)) {
      ent.items = await buildLsDir(fs, ctx, `${dir.replace(/\/$/, "")}/${name}`, remain - 1, all, state);
    }
    out.push(ent);
  }
  return out;
}

// sortLsEntries 逐级排序：name = UTF-8 字节序；time = mtime 降序（同值按名称升序，稳定）。
function sortLsEntries(items, byTime) {
  if (byTime) {
    items
      .map((it, i) => ({ it, i }))
      .sort((a, b) => {
        const d = (b.it.mod_time || 0) - (a.it.mod_time || 0);
        if (d !== 0) return d;
        return cmpBytes(a.it.name, b.it.name);
      })
      .forEach((x, i) => (items[i] = x.it));
  } else {
    items.sort((a, b) => cmpBytes(a.name, b.name));
  }
  for (const it of items) {
    if (it.items) sortLsEntries(it.items, byTime);
  }
}

// ---- rg（对齐 vcore rg.go：内容搜索 / files 列举）----

// RG_UNSUPPORTED_RE：Rust regex 同族不支持（lookaround/backreference），
// 命中即受限反馈并引导 shell 逃生舱（与 Go rgUnsupportedPatterns 一致）。
const RG_UNSUPPORTED_RE = /(\(\?<?[=!])|(\\[1-9])/;
const RG_UNSUPPORTED_HINT = "pattern is not supported on this environment (restricted: no lookaround/backreference), use bash -c \"grep -P ...\" on a physical host";

async function fsRg(fs, ctx, p) {
  const globs = Array.isArray(p.glob) ? p.glob.map(String) : [];
  for (const g of globs) {
    if (g.includes("!") || g.includes("**")) {
      throw fsErr("rg", `glob "${g}" is not supported on this environment (restricted: no '!' negation or '**')`);
    }
  }

  // files 模式：纯文件列举，不接受搜索参数
  if (p.files) {
    if (p.pattern || p.insensitive || p.files_only || p.count || p.word || (p.max_per_file ?? 0) > 0) {
      throw fsErr("rg", "files mode cannot be combined with search params (pattern, insensitive, files_only, count, word, max_per_file)");
    }
    return rgFiles(fs, ctx, p.path || defaultTarget(ctx), globs, !!p.hidden);
  }

  // 搜索模式：pattern 必填，path 缺省 = workdir
  if (!p.pattern) throw fsErr("rg", "pattern is required (or set files=true to list files)");
  const pattern = String(p.pattern);
  if (RG_UNSUPPORTED_RE.test(pattern)) throw fsErr("rg", RG_UNSUPPORTED_HINT);
  let src = pattern;
  if (p.word) src = `\\b(?:${src})\\b`;
  let re;
  try {
    re = new RegExp(src, p.insensitive ? "i" : "");
  } catch (e) {
    throw fsErr("rg", `invalid pattern: ${e.message}`);
  }
  let maxPerFile = 0;
  if (p.max_per_file !== undefined && p.max_per_file !== null) {
    maxPerFile = Number(p.max_per_file);
    if (!Number.isInteger(maxPerFile) || maxPerFile < 1) {
      throw fsErr("rg", `max_per_file must be >= 1, got ${p.max_per_file}`);
    }
  }

  const target = p.path || defaultTarget(ctx);
  const abs = await absOf(fs, target, ctx);
  const st = await fs.stat(target, ctx);
  if (st === null) throw fsErr("rg", `${abs}: no such file or directory`);
  let candidates;
  if (!st.dir) {
    candidates = [target];
  } else {
    candidates = await rgWalk(fs, ctx, target, globs, !!p.hidden);
  }
  return rgSearch(fs, ctx, abs, pattern, candidates, re, maxPerFile, !!p.files_only, !!p.count);
}

// rgWalk 递归收集文件：目标前缀下任一路径段命中隐藏（hidden 收录）或
// RG_SKIP_DIRS 即跳过（与 Go rgWalk 的递归跳过语义一致）；glob 按文件名 OR 过滤。
async function rgWalk(fs, ctx, target, globs, hidden) {
  const w = await fs.walk(target, ctx);
  const prefix = target.endsWith("/") ? target : target + "/";
  const out = [];
  for (const it of w.items || []) {
    if (it.dir) continue;
    const rel = it.path.startsWith(prefix) ? it.path.slice(prefix.length) : it.path;
    const segs = rel.split("/");
    if (segs.some((s) => RG_SKIP_DIRS.has(s))) continue;
    if (!hidden && segs.some((s) => isHidden(s))) continue;
    const name = segs[segs.length - 1] || "";
    if (globs.length && !globs.some((g) => globMatch(g, name))) continue;
    out.push(it.path);
  }
  out.sort(cmpBytes);
  return out;
}

async function rgFiles(fs, ctx, target, globs, hidden) {
  const abs = await absOf(fs, target, ctx);
  const st = await fs.stat(target, ctx);
  if (st === null) throw fsErr("rg", `${abs}: no such file or directory`);
  let files;
  if (!st.dir) {
    files = [abs];
  } else {
    files = await rgWalk(fs, ctx, target, globs, hidden);
  }
  let truncated = files.length > RG_DEFAULT_LIMIT;
  if (truncated) files = files.slice(0, RG_DEFAULT_LIMIT);
  if (files.length === 0) {
    const msg = globs.length ? `no files matched globs ${globs.join(", ")} in ${abs}` : `no files found in ${abs}`;
    return { content: msg, attrs: { action: "rg", path: abs, rows: "0", truncated: "false" } };
  }
  let content = "";
  let bytes = 0;
  let out = 0;
  const enc = new TextEncoder();
  for (const f of files) {
    const line = f + "\n";
    const bl = enc.encode(line).length;
    if (bytes + bl > RG_MAX_CONTENT_BYTES) break;
    content += line;
    bytes += bl;
    out++;
  }
  content = content.replace(/\n$/, "");
  if (out < files.length) truncated = true;
  return { content, attrs: { action: "rg", path: abs, rows: String(out), truncated: String(truncated) } };
}

// clipRgText 按字节截断超长匹配行内容（UTF-8 边界收刀），超限追加标记
// （与 vcore clipRgText 一致）。
function clipRgText(text) {
  const enc = new TextEncoder();
  const bytes = enc.encode(text);
  if (bytes.length <= RG_MAX_LINE_BYTES) return { text, clipped: false };
  let cut = RG_MAX_LINE_BYTES;
  const dec = new TextDecoder("utf-8", { fatal: true });
  for (; cut > 0; cut--) {
    try {
      dec.decode(bytes.slice(0, cut));
      break;
    } catch (e) {
      /* 截断点在多字节字符内，回退 */
    }
  }
  return { text: new TextDecoder().decode(bytes.slice(0, cut)) + "...[truncated]", clipped: true };
}

async function rgSearch(fs, ctx, abs, pattern, candidates, re, maxPerFile, filesOnly, countOnly) {
  const rows = [];
  let truncated = false;
  let clipped = false;
  for (const f of candidates) {
    if (rows.length >= RG_DEFAULT_LIMIT) {
      truncated = true;
      break;
    }
    const quota = RG_DEFAULT_LIMIT - rows.length;
    let perFile = maxPerFile;
    if (filesOnly) perFile = 1;
    else if (countOnly) perFile = Infinity;
    if (!countOnly && (perFile <= 0 || perFile > quota)) perFile = quota;
    const ms = await rgFile(fs, ctx, f, re, perFile);
    if (!ms || ms.length === 0) continue;
    if (filesOnly) {
      rows.push(f);
      continue;
    }
    if (countOnly) {
      rows.push(`${f}:${ms.length}`);
      continue;
    }
    for (const m of ms) {
      const { text, clipped: c } = clipRgText(m.text);
      if (c) clipped = true;
      rows.push(`${m.path}:${m.line}:${text}`);
    }
  }
  if (rows.length === 0) {
    return { content: `no matches for pattern "${pattern}" in ${abs}`, attrs: { action: "rg", path: abs, rows: "0", truncated: "false" } };
  }
  if (clipped) truncated = true;
  // 512KB 字节预算只留完整行（§2.5；首行同样受检）
  let content = "";
  let bytes = 0;
  let out = 0;
  const enc = new TextEncoder();
  for (const row of rows) {
    const line = row + "\n";
    const bl = enc.encode(line).length;
    if (bytes + bl > RG_MAX_CONTENT_BYTES) break;
    content += line;
    bytes += bl;
    out++;
  }
  content = content.replace(/\n$/, "");
  if (out < rows.length) truncated = true;
  return { content, attrs: { action: "rg", path: abs, rows: String(out), truncated: String(truncated) } };
}

async function rgFile(fs, ctx, path, re, max) {
  const raw = await fs.readRaw(path, ctx);
  if (!raw || (raw.mime && raw.mime !== "text/plain")) return null; // 二进制跳过
  const text = String(raw.content ?? "");
  // 剥除尾随换行产生的空元素，避免 ^$ 等空串模式幻影报出文件末尾一行
  const lines = text.split("\n");
  if (lines.length && lines[lines.length - 1] === "") lines.pop();
  const out = [];
  for (let i = 0; i < lines.length; i++) {
    const line = lines[i].replace(/\r$/, ""); // CRLF 统一剥除
    if (re.test(line)) {
      out.push({ path, line: i + 1, text: line });
      if (out.length >= max) break;
    }
  }
  return out;
}

// ---- cp / mv / rm（对齐 vcore fileops.go）----

// copyNode 复制单个文件/整棵目录树（PageFS 无原生 Rename/MkdirAll——writeBlob 自动建父路径）。
async function copyNode(fs, ctx, srcAbs, dstAbs) {
  const st = await fs.stat(srcAbs, ctx);
  if (st === null) throw fsErr("cp", `cannot stat source ${srcAbs}: no such file or directory`);
  if (!st.dir) {
    const r = await fs.readRaw(srcAbs, ctx);
    const blob = r.content instanceof Blob ? r.content : new Blob([r.content], { type: r.mime || "text/plain" });
    await fs.writeBlob(dstAbs, blob, ctx);
    return;
  }
  const w = await fs.walk(srcAbs, ctx);
  for (const it of w.items || []) {
    if (it.dir) continue;
    const rel = it.path.slice(srcAbs.length).replace(/^\/+/, "");
    const r = await fs.readRaw(it.path, ctx);
    const blob = r.content instanceof Blob ? r.content : new Blob([r.content], { type: r.mime || "text/plain" });
    await fs.writeBlob(dstAbs + "/" + rel, blob, ctx);
  }
}

async function fsCp(fs, ctx, p) {
  if (!p.src || !p.dst) throw fsErr("cp", "src and dst are required");
  const srcAbs = await absOf(fs, p.src, ctx);
  const dstAbs = await absOf(fs, p.dst, ctx);
  if (srcAbs === dstAbs) throw fsErr("cp", `${srcAbs} and ${dstAbs} are identical`);
  const st = await fs.stat(p.src, ctx);
  if (st === null) throw fsErr("cp", `cannot stat source ${srcAbs}: no such file or directory`);
  if (st.dir) {
    if (!p.recursive) throw fsErr("cp", `${srcAbs} is a directory (set recursive=true)`);
    if (dstAbs.startsWith(srcAbs + "/")) throw fsErr("cp", `cannot copy directory ${srcAbs} into itself: ${dstAbs}`);
  }
  if ((await fs.stat(p.dst, ctx)) !== null) throw fsErr("cp", `destination ${dstAbs} already exists`);
  await copyNode(fs, ctx, srcAbs, dstAbs);
  return { content: `copied ${srcAbs} to ${dstAbs}`, attrs: { action: "cp", path: dstAbs, source_path: srcAbs } };
}

async function fsMv(fs, ctx, p) {
  if (!p.src || !p.dst) throw fsErr("mv", "src and dst are required");
  const srcAbs = await absOf(fs, p.src, ctx);
  const dstAbs = await absOf(fs, p.dst, ctx);
  if (srcAbs === dstAbs) throw fsErr("mv", `${srcAbs} and ${dstAbs} are identical`);
  const st = await fs.stat(p.src, ctx);
  if (st === null) throw fsErr("mv", `cannot stat source ${srcAbs}: no such file or directory`);
  if (st.dir && dstAbs.startsWith(srcAbs + "/")) throw fsErr("mv", `cannot move directory ${srcAbs} into itself: ${dstAbs}`);
  if ((await fs.stat(p.dst, ctx)) !== null) throw fsErr("mv", `destination ${dstAbs} already exists`);
  await copyNode(fs, ctx, srcAbs, dstAbs);
  try {
    await fs.remove(p.src, { ...ctx, recursive: true });
  } catch (e) {
    throw fsErr("mv", `cannot remove source ${srcAbs} after copy: ${e?.message || e}`);
  }
  return { content: `moved ${srcAbs} to ${dstAbs}`, attrs: { action: "mv", path: dstAbs, source_path: srcAbs } };
}

async function fsRm(fs, ctx, p) {
  if (!p.path) throw fsErr("rm", "path is required");
  const abs = await absOf(fs, p.path, ctx);
  const st = await fs.stat(p.path, ctx);
  if (st === null) throw fsErr("rm", `${abs}: no such file or directory`);
  let out;
  if (!st.dir) {
    out = await fs.remove(p.path, ctx);
  } else {
    // 目录：非空需 recursive（PageFS.remove 对空目录与不存在均报 no such file）
    const res = await fs.list(p.path, ctx);
    const children = (res.items || []).length;
    if (children > 0 && !p.recursive) {
      throw fsErr("rm", `${abs} is a non-empty directory (set recursive=true)`);
    }
    out = await fs.remove(p.path, { ...ctx, recursive: !!p.recursive });
  }
  const attrs = { action: "rm", path: out.removed };
  if (out.items) attrs.items = String(out.items);
  return {
    content: out.items ? `removed ${out.removed} (${out.items} items)` : `removed ${out.removed}`,
    attrs,
  };
}

// ---- 入口 ----

// runFsOps(fs, params, ctx) → {content, attrs}；错误 throw Error("fs {action}: {原因}")。
// params = fs JSON 参数（action 为 ls/rg/cp/mv/rm 之一）；ctx: {workdir?}。
export async function runFsOps(fs, params, ctx = {}) {
  const action = String(params?.action || "");
  switch (action) {
    case "ls":
      return fsLs(fs, ctx, params);
    case "rg":
      return fsRg(fs, ctx, params);
    case "cp":
      return fsCp(fs, ctx, params);
    case "mv":
      return fsMv(fs, ctx, params);
    case "rm":
      return fsRm(fs, ctx, params);
  }
  throw fsErr("", `unknown action "${action}" (supported: read, write, edit, ls, rg, cp, mv, rm)`);
}
