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
// RG_SKIP_DIRS 是 rg 恒跳过集（与 vcore skipDirs 一致；与 LS_SKIP_DIRS 同一集合）。
const RG_SKIP_DIRS = new Set([
  "node_modules", "vendor", "__pycache__", "bower_components", "dist", "build",
  "target", ".next", ".nuxt", "coverage", ".turbo", ".output",
]);

const LS_DEFAULT_DEPTH = 1;
const LS_MAX_DEPTH = 5;
const LS_MAX_NODES = 2000;
const RG_DEFAULT_LIMIT = 50;
const RG_MAX_LIMIT = 200;
const RG_MAX_CONTEXT = 10;
// RG_MAX_LINE_BYTES 匹配行内容单行字节上限 / RG_MAX_CONTENT_BYTES 总预算
// （对齐 vcore rg.go §2.5：minified 单行超长命中不撑爆上下文）。
const RG_MAX_LINE_BYTES = 4 << 10;
const RG_MAX_CONTENT_BYTES = 128 << 10;

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
  sortLsEntries(items);
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

// sortLsEntries 逐级排序：mtime 降序（最近修改在前），同值按名称 UTF-8 字节序（稳定）。
function sortLsEntries(items) {
  items
    .map((it, i) => ({ it, i }))
    .sort((a, b) => {
      const d = (b.it.mod_time || 0) - (a.it.mod_time || 0);
      if (d !== 0) return d;
      return cmpBytes(a.it.name, b.it.name);
    })
    .forEach((x, i) => (items[i] = x.it));
  for (const it of items) {
    if (it.items) sortLsEntries(it.items);
  }
}

// ---- rg（对齐 vcore rg.go：内容搜索 / files 列举）----

// RG_UNSUPPORTED_RE：Rust regex 同族不支持（lookaround/backreference），
// 命中即受限反馈并引导 shell 逃生舱（与 Go rgUnsupportedPatterns 一致）。
const RG_UNSUPPORTED_RE = /(\(\?<?[=!])|(\\[1-9])/;
const RG_UNSUPPORTED_HINT = "pattern is not supported on this environment (restricted: no lookaround/backreference), use bash -c \"grep -P ...\" on a physical host";

// MINIFIED 判定（§4.6，与 vcore 一致）：三级判定。①命名约定快路径：
// *.min.js/*.min.css/*.min.mjs 后缀即压缩语义（零 I/O）；②采样快路径：size ≥
// MINIFIED_MIN_BYTES 且采样前 MINIFIED_HEAD_BYTES 字节内换行 < MINIFIED_MIN_LINES
// → 压缩/单行文件，默认跳过（all 收录，显式单文件路径不跳过）；③可疑区间兜底：
// 头部含长 license 注释等使采样内换行 ≥32 的压缩库（如 echarts.min.js：1MB/45
// 行）且采样换行 < MINIFIED_SUSPECT_LINES 时，用全文件平均行长判定——
// size/行数 > MINIFIED_AVG_LINE_BYTES 仍判 minified。采样换行 ≥ 可疑区间上界必
// 为正常文件（真实分布：正常 ≥1250、压缩 ≤34），不做全文件统计。
const MINIFIED_HEAD_BYTES = 64 << 10;
const MINIFIED_MIN_LINES = 32;
const MINIFIED_MIN_BYTES = 32 << 10;
const MINIFIED_SUSPECT_LINES = 256; // 可疑区间上界：≥ 此值必为正常文件
const MINIFIED_AVG_LINE_BYTES = 1 << 10; // 1KB/行：正常格式化代码平均行长远低于此
const MINIFIED_NAME_RE = /\.min\.(js|css|mjs)$/;

// isMinifiedRaw 判定 raw 是否为 minified/单行超长（与 vcore isMinified 一致）。
// ①命名约定（raw.path 后缀 .min.js 等，零额外 I/O）；②采样快路径：前 64KB 字节
// 内换行 < 32；③可疑区间（采样换行 < 256）平均行长兜底 > 1KB/行（readRaw 已
// 整读，indexOf 循环数换行无额外内存）。二进制 Blob/读失败按非 minified 处理。
function isMinifiedRaw(raw) {
  if (!raw) return false;
  if (MINIFIED_NAME_RE.test(raw.path ?? "")) return true; // 命名约定快路径（零 I/O，优先于 size，与 Go 一致）
  if (typeof raw.size !== "number" || raw.size < MINIFIED_MIN_BYTES) return false;
  if (typeof raw.content !== "string") return false; // 二进制 Blob 不判
  const enc = new TextEncoder();
  const bytes = enc.encode(raw.content.slice(0, MINIFIED_HEAD_BYTES));
  // 采样收紧到 64KB 字节（slice 是 code unit 索引，多字节内容可能超采样窗口）
  const sample = bytes.length > MINIFIED_HEAD_BYTES ? bytes.subarray(0, MINIFIED_HEAD_BYTES) : bytes;
  let nl = 0;
  for (const b of sample) if (b === 10) nl++;
  if (nl < MINIFIED_MIN_LINES) return true;
  if (nl >= MINIFIED_SUSPECT_LINES) return false; // 正常格式化文件，跳过全文件统计
  // 可疑区间：全文件平均行长兜底（\n 计数与 Go bytes.Count 一致；
  // Math.floor 对齐 Go int64 整除，双端边界判定一致）
  let totalNl = 0;
  let idx = -1;
  while ((idx = raw.content.indexOf("\n", idx + 1)) !== -1) totalNl++;
  return totalNl > 0 && Math.floor(raw.size / totalNl) > MINIFIED_AVG_LINE_BYTES;
}

async function fsRg(fs, ctx, p) {
  const globs = Array.isArray(p.glob) ? p.glob.map(String) : [];
  for (const g of globs) {
    if (g.replace(/^!/, "").includes("**")) {
      throw fsErr("rg", `glob "${g}" is not supported on this environment (restricted: no '**')`);
    }
  }
  let limit = RG_DEFAULT_LIMIT;
  if (p.limit !== undefined && p.limit !== null) {
    limit = Number(p.limit);
    if (!Number.isInteger(limit) || limit < 1 || limit > RG_MAX_LIMIT) {
      throw fsErr("rg", `limit must be between 1 and ${RG_MAX_LIMIT}, got ${p.limit}`);
    }
  }
  let rgCtx = 0;
  if (p.context !== undefined && p.context !== null) {
    rgCtx = Number(p.context);
    if (!Number.isInteger(rgCtx) || rgCtx < 0 || rgCtx > RG_MAX_CONTEXT) {
      throw fsErr("rg", `context must be between 0 and ${RG_MAX_CONTEXT}, got ${p.context}`);
    }
  }

  const target = p.path || defaultTarget(ctx);

  // pattern 缺省 = 列举模式：纯文件列举（不接受搜索语义）
  if (!p.pattern) {
    if (rgCtx > 0) throw fsErr("rg", "context is only valid for content search (pattern is required)");
    return rgFiles(fs, ctx, target, globs, !!p.all, limit);
  }

  const pattern = String(p.pattern);
  if (RG_UNSUPPORTED_RE.test(pattern)) throw fsErr("rg", RG_UNSUPPORTED_HINT);
  // smart case：pattern 不含大写字母 → 大小写不敏感（ripgrep 惯例，与 Go 端一致）
  const flags = /\p{Lu}/u.test(pattern) ? "" : "i";
  let re;
  try {
    re = new RegExp(pattern, flags);
  } catch (e) {
    throw fsErr("rg", `invalid pattern: ${e.message}`);
  }

  const abs = await absOf(fs, target, ctx);
  const st = await fs.stat(target, ctx);
  if (st === null) throw fsErr("rg", `${abs}: no such file or directory`);
  let candidates;
  if (!st.dir) {
    // 显式单文件路径：不做 minified 跳过（用户显式指定即明确意图）
    candidates = [target];
    return rgSearch(fs, ctx, abs, pattern, candidates, re, limit, rgCtx, true);
  }
  candidates = await rgWalk(fs, ctx, target, globs, !!p.all);
  return rgSearch(fs, ctx, abs, pattern, candidates, re, limit, rgCtx, !!p.all);
}

// rgWalk 递归收集文件：目标前缀下任一路径段命中隐藏（hidden 收录）或
// RG_SKIP_DIRS 即跳过（与 Go rgWalk 的递归跳过语义一致）；glob 按文件名过滤
// （include OR + ! 排除，与 Go globOK 一致）。
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
    if (globs.length && !rgGlobOK(globs, name)) continue;
    out.push(it.path);
  }
  out.sort(cmpBytes);
  return out;
}

// rgGlobOK：include glob OR 任一命中即通过；! 前缀 = 排除 glob（命中任一
// 排除即不通过）；无 include = 全通过（与 Go globOK 一致）。
function rgGlobOK(globs, name) {
  const inc = [];
  for (const g of globs) {
    if (g.startsWith("!")) {
      if (globMatch(g.slice(1), name)) return false;
      continue;
    }
    inc.push(g);
  }
  if (!inc.length) return true;
  return inc.some((g) => globMatch(g, name));
}

async function rgFiles(fs, ctx, target, globs, hidden, limit) {
  const abs = await absOf(fs, target, ctx);
  const st = await fs.stat(target, ctx);
  if (st === null) throw fsErr("rg", `${abs}: no such file or directory`);
  let files;
  if (!st.dir) {
    files = [abs];
  } else {
    files = await rgWalk(fs, ctx, target, globs, hidden);
  }
  let truncated = files.length > limit;
  if (truncated) files = files.slice(0, limit);
  const jb = makeRGJSONBuf(`]}`); // 预留数组+顶层闭合（2 字节）
  jb.write(`{"files":[`);
  let rows = 0;
  for (let i = 0; i < files.length; i++) {
    let s = jsonStr(files[i]);
    if (i > 0) s = "," + s; // 逗号与元素原子写入，防悬空逗号
    if (!jb.write(s)) {
      truncated = true;
      break;
    }
    rows++;
  }
  jb.close(`]}`);
  return { content: jb.done(), attrs: { action: "rg", path: abs, rows: String(rows), truncated: String(truncated) } };
}

// rgNoteMax 是 note 尾部的最坏情况长度（skipped 为 int64 最大值），用于
// rgSearch 的 makeRGJSONBuf 预留收尾空间（与 vcore rgNoteMax 一致）。
const RG_NOTE_MAX = `],"note":"9223372036854775807 minified files skipped, use all=true to include"}`;

// makeRGJSONBuf 预算感知增量 JSON 构建（与 vcore rgJSONBuf 一致）：构建时
// 预留尾部闭合（含可选 note）空间，内容写满即截断（write 返回 false）但
// close 恒成功——输出恒为合法 JSON 且 UTF-8 字节 ≤ RG_MAX_CONTENT_BYTES。
function makeRGJSONBuf(tailMax) {
  const enc = new TextEncoder();
  let buf = "";
  let bytes = 0;
  let truncated = false;
  const limit = RG_MAX_CONTENT_BYTES - enc.encode(tailMax).length;
  return {
    // write 追加内容；超预算返回 false 并置 truncated（此后 write 全部拒绝）
    write(s) {
      if (truncated) return false;
      const bl = enc.encode(s).length;
      if (bytes + bl > limit) {
        truncated = true;
        return false;
      }
      buf += s;
      bytes += bl;
      return true;
    },
    // close 强制写入收尾（调用方保证 end 长度 ≤ 预留 tailMax）
    close(end) {
      buf += end;
    },
    done() {
      return buf;
    },
  };
}

// jsonStr 输出 JSON 字符串字面量，并模拟 Go json.Marshal 的 HTML 转义
// （< > & → \u003c 等），保证双端字节一致。
function jsonStr(s) {
  return JSON.stringify(s).replace(/[<>&]/g, (c) =>
    c === "<" ? "\\u003c" : c === ">" ? "\\u003e" : "\\u0026");
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

async function rgSearch(fs, ctx, abs, pattern, candidates, re, limit, rgCtx, includeMinified) {
  // 预留最大 note 空间：内容写满即截断，收尾（数组闭合+可选 note）恒可写
  const jb = makeRGJSONBuf(RG_NOTE_MAX);
  jb.write(`{"files":[`);
  let contentRows = 0;
  let truncated = false;
  let clipped = false;
  let skipped = 0;
  let firstFile = true;
  for (const f of candidates) {
    if (contentRows >= limit) {
      truncated = true;
      break;
    }
    // 一次 readRaw 同时服务 minified 判定与行扫描（避免每文件两遍全量读）
    const raw = await fs.readRaw(f, ctx);
    if (!raw) continue; // 读失败跳过
    if (!includeMinified && isMinifiedRaw(raw)) {
      skipped++;
      continue;
    }
    const lines = linesOfRaw(raw);
    if (!lines) continue; // 二进制跳过
    const res = rgEmitRows(lines, re, rgCtx, limit - contentRows);
    if (res.truncated) truncated = true;
    // 文件级原子 chunk：逗号+整文件整体写入预算检查，超限丢弃（JSON 恒闭合）
    let fb = `{"path":${jsonStr(f)},"matches":[`;
    let fRows = 0;
    let firstRow = true;
    for (const r of res.rows) {
      if (r.sep) continue; // 结构化后组间分隔无意义（行序即上下文序）
      const { text, clipped: c } = clipRgText(r.text);
      if (c) clipped = true;
      if (!firstRow) fb += ",";
      firstRow = false;
      fb += `{"line":${r.line},"text":${jsonStr(text)}`;
      if (!r.match) fb += `,"ctx":true`;
      fb += `}`;
      fRows++;
    }
    if (!fRows) continue;
    let chunk = fb + `]}`;
    if (!firstFile) chunk = "," + chunk;
    if (!jb.write(chunk)) {
      truncated = true;
      break;
    }
    firstFile = false;
    contentRows += fRows;
  }
  if (skipped > 0) jb.close(`],"note":"${skipped} minified files skipped, use all=true to include"}`);
  else jb.close(`]}`);
  if (clipped) truncated = true;
  const attrs = { action: "rg", path: abs, rows: String(contentRows), truncated: String(!!(truncated || clipped)) };
  if (skipped > 0) attrs.skipped = String(skipped);
  return { content: jb.done(), attrs };
}

// rgEmitRows 单遍扫描+上下文展开（GNU grep -C 语义，与 Go rgEmit 一致）：
// 命中行前 rgCtx 行缓冲补发、后 rgCtx 行计数直发；上下文区相邻或重叠
// （两命中间无未选行）合并为一个组，组间插入 --；内容行数（不含 --）达到
// maxRows 后仅探测剩余命中决定 truncated。
function rgEmitRows(lines, re, rgCtx, maxRows) {
  const rows = [];
  let pending = []; // before-context 缓冲（至多 rgCtx 行）
  let lastEmitted = 0; // 最近已输出内容行号（0 = 组未开）
  let after = 0;
  let n = 0;
  let truncated = false;
  for (let i = 0; i < lines.length; i++) {
    const isMatch = re.test(lines[i]);
    if (n >= maxRows) {
      // 预算已满：只探测剩余行是否还有命中（决定 truncated）
      if (isMatch) {
        truncated = true;
        break;
      }
      continue;
    }
    if (isMatch) {
      // 组间断判定：新命中（或其 before 缓冲首行）与上一输出行不衔接 → --
      const start = pending.length ? pending[0].line : i + 1;
      if (rgCtx > 0 && lastEmitted > 0 && start !== lastEmitted + 1) {
        rows.push({ sep: true });
      }
      let stop = false;
      for (const pr of pending) {
        if (n >= maxRows) {
          truncated = true;
          stop = true;
          break;
        }
        rows.push(pr);
        n++;
        lastEmitted = pr.line;
      }
      pending = [];
      if (stop) break;
      if (n >= maxRows) {
        // 命中行自身放不下：截断（存在被压制的命中）
        truncated = true;
        break;
      }
      rows.push({ line: i + 1, text: lines[i], match: true });
      n++;
      lastEmitted = i + 1;
      after = rgCtx;
      continue;
    }
    if (after > 0) {
      rows.push({ line: i + 1, text: lines[i] });
      n++;
      lastEmitted = i + 1;
      after--;
    } else if (rgCtx > 0) {
      pending.push({ line: i + 1, text: lines[i] });
      if (pending.length > rgCtx) pending.shift();
    }
  }
  return { rows, truncated };
}

// linesOfRaw 将 raw 拆为行数组（剥尾随空元素与行尾 \r）；二进制/非文本 → null。
function linesOfRaw(raw) {
  if (!raw || (raw.mime && raw.mime !== "text/plain")) return null;
  if (typeof raw.content !== "string") return null;
  const lines = raw.content.split("\n");
  if (lines.length && lines[lines.length - 1] === "") lines.pop();
  for (let i = 0; i < lines.length; i++) lines[i] = lines[i].replace(/\r$/, "");
  return lines;
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
