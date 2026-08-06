// vcmd.js — page 端核心虚拟指令（§5.4，与 aic-pod/sdk/vcore Go 实现语义对齐）。
// 双端共用一份（aic-pod/browser/src/sdk/vcmd.js ⇆ aic/ui/assets/libs/vcmd.js 逐字节同步）。
//
// 指令：ls / rg / tree / rm / curl（curl 走 fetch，CORS 失败按执行错误）。
// 行为基准 = vcore Go（ls.go/rg.go/tree.go/fileops.go/curl.go）：
//   - 单横线 flag 解析 + 组合展开（-la）+ 受限反馈（未列 flag 一律拒绝）
//   - 输出文本/JSON 格式与 attrs 键对齐 vcore Result
//   - 错误消息格式锁定：{cmd}: {原因}（status=error 由上层统一包装）
//
// fs 适配器接口（PageFS 实现，vcmd 不依赖 IndexedDB，可注入 mock 测试）：
//   stat(p, ctx)     → {path, dir, size, mtime} | null
//   list(p, ctx)     → {items: [{name, dir, size, mtime}]}（一层，目录推导）
//   walk(p, ctx)     → {items: [{path, dir, size, mtime}]}（递归平铺，不过滤）
//   readRaw(p, ctx)  → {content, mime, size, path}（rg 内容搜索：非 text 跳过）
//   writeBlob(p, blob, ctx) → {ok, path, size}（curl 落盘，Blob 保留原始字节）

// ---- 受限反馈 / 错误 ----

// execErr 构造虚拟指令错误消息：{cmd}: {原因}（§5.4 基准，不带 exec 前缀）。
function execErr(cmd, msg) {
  return new Error(`${cmd}: ${msg}`);
}

const RESTRICTED = "is not supported on this environment (restricted)";

// ---- argv 解析（对齐 vcore parseArgv，§5.4）----

// parseVCmdArgv(cmd, spec, argv)：
//   spec: {bools: {flag: true}, values: {flag: true}, lists: {flag: true}, minPos, maxPos}
//   返回 {bools, values, lists, pos}；未知 flag / 组合含未知 / 位置参数越界 → 受限反馈。
export function parseVCmdArgv(cmd, spec, argv) {
  // 归一化：Go 端 argvSpec 的 values/lists 为零值 nil map（读取安全），
  // JS 未传字段为 undefined——统一补齐避免 TypeError。
  spec = {
    bools: spec.bools || {},
    values: spec.values || {},
    lists: spec.lists || {},
    minPos: spec.minPos || 0,
    maxPos: spec.maxPos === undefined ? -1 : spec.maxPos,
  };
  const out = { bools: {}, values: {}, lists: {}, pos: [] };
  for (let i = 0; i < argv.length; i++) {
    const a = argv[i];
    if (a.startsWith("-") && a !== "-") {
      if (a.includes("=")) {
        throw execErr(cmd, `flag "${a}" is not supported on this environment (restricted; use "--flag value" two-element form)`);
      }
      if (spec.bools[a]) {
        out.bools[a] = true;
        continue;
      }
      if (spec.values[a]) {
        if (i + 1 >= argv.length) throw execErr(cmd, `flag "${a}" requires a value`);
        out.values[a] = argv[++i];
        continue;
      }
      if (spec.lists[a]) {
        if (i + 1 >= argv.length) throw execErr(cmd, `flag "${a}" requires a value`);
        out.lists[a] = (out.lists[a] || []).concat(argv[++i]);
        continue;
      }
      // 单横线组合（-la）：全部为已知无值 flag 时展开（对齐真实命令）
      if (a.length > 2 && !a.startsWith("--")) {
        let ok = true;
        const flags = [];
        for (const c of a.slice(1)) {
          const f = "-" + c;
          if (!spec.bools[f]) {
            ok = false;
            break;
          }
          flags.push(f);
        }
        if (ok) {
          for (const f of flags) out.bools[f] = true;
          continue;
        }
      }
      throw execErr(cmd, `flag "${a}" ${RESTRICTED}`);
    }
    out.pos.push(a);
  }
  if (out.pos.length < spec.minPos) {
    throw execErr(cmd, `missing argument (expected at least ${spec.minPos}, got ${out.pos.length})`);
  }
  if (spec.maxPos >= 0 && out.pos.length > spec.maxPos) {
    throw execErr(cmd, `unexpected argument "${out.pos[spec.maxPos]}"`);
  }
  return out;
}

// ---- 公共工具（对齐 vcore）----

// cmpBytes 按 UTF-8 字节序比较两个字符串（对齐 vcore 排序，§5.4：
// 禁止 locale 相关排序）。返回负数/0/正数。
export function cmpBytes(a, b) {
  const ae = new TextEncoder().encode(a);
  const be = new TextEncoder().encode(b);
  const n = Math.min(ae.length, be.length);
  for (let i = 0; i < n; i++) {
    if (ae[i] !== be[i]) return ae[i] - be[i];
  }
  return ae.length - be.length;
}

const SKIP_DIRS = new Set([
  "node_modules",
  "vendor",
  "__pycache__",
  "bower_components",
  "dist",
  "build",
  "target",
  ".next",
  ".nuxt",
  "coverage",
  ".turbo",
  ".output",
]);
const TREE_SKIP_DIRS = SKIP_DIRS; // tree 不 descend 集（叶条目保留）
const RG_SKIP_DIRS = new Set(["node_modules", "vendor"]); // rg 恒跳过集（对齐 Go skipDirs）

// 默认隐藏点开头（ls/rg 对齐真实命令；tree 完全跳过）。
function isHidden(name) {
  return name.startsWith(".");
}

// humanSize 对齐 GNU ls -h（1024 进制单字母后缀，1 位小数）。
export function humanSize(n) {
  if (n < 1024) return `${n}B`;
  let div = 1024;
  let exp = 0;
  for (let m = Math.floor(n / 1024); m >= 1024; m = Math.floor(m / 1024)) {
    div *= 1024;
    exp++;
  }
  return `${(n / div).toFixed(1)}${"KMGTPE"[exp]}`;
}

// globMatch：rg -g 文件名 glob（* 任意序列 / ? 单字符，完整匹配，线性双指针）。
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

// rgUnsupportedPatternRe：Rust regex 同族不支持（lookaround/backreference），
// 命中即受限反馈并引导 shell 逃生舱（与 Go rgUnsupportedPatterns 一致）。
const RG_UNSUPPORTED_RE = /(\(\?<?[=!])|(\\[1-9])/;

// defaultTarget 指令缺省目标路径：显式 path > ctx.workdir > 本地根 /（v0.13.1 单根，
// page/扩展本地空间无会话概念）。
export function defaultTarget(ctx) {
  return ctx?.workdir || "/";
}

// ---- ls（对齐 vcore ls.go）----

// ls [-l] [-a] [-t] [-h] [path]：默认隐藏点文件；-l size/mtime 列；-h 人类可读
// 大小（1024 进制，仅 -l 生效）；-t mtime 降序（同 mtime 按名称升序）；单文件输出
// 文件名；空目录 "empty directory: {abs}"。目录 size/mtime 缺失（IndexedDB 无目录
// 记录）时输出 "-"（数据模型限制，Go UFS 端为真实数值）。
export async function cmdLs(fs, ctx, argv) {
  const pa = parseVCmdArgv("ls", { bools: { "-l": true, "-a": true, "-t": true, "-h": true }, minPos: 0, maxPos: 1 }, argv);
  const target = pa.pos[0] || defaultTarget(ctx);
  const abs = fs._path ? await fs._path(target, ctx) : target;
  const st = await fs.stat(target, ctx);
  if (st === null) throw execErr("ls", `${abs}: no such file or directory`);
  const long = pa.bools["-l"];
  const human = pa.bools["-h"];
  const all = pa.bools["-a"];
  const byTime = pa.bools["-t"];

  const fmtLine = (name, dir, size, mt) => {
    const label = dir ? `${name}/` : name;
    if (!long) return label;
    if (human) {
      const sz = dir ? "-" : (size === undefined ? "-" : humanSize(size));
      const t = mt === undefined ? "-" : Math.floor(mt / 1000);
      return `${label}\t${sz}\t${t}`;
    }
    const sz = dir ? "-" : (size === undefined ? "-" : String(size));
    const t = mt === undefined ? "-" : Math.floor(mt / 1000);
    return `${label}\t${sz}\t${t}`;
  };

  if (!st.dir) {
    // 单文件：输出文件名（对齐真实 ls）
    const name = st.path.split("/").pop() || st.path;
    return { content: fmtLine(name, false, st.size, st.mtime), attrs: { action: "ls", path: abs, path_kind: "file", rows: "1", truncated: "false" } };
  }

  const res = await fs.list(target, ctx);
  let items = res.items || [];
  if (!all) items = items.filter((it) => !isHidden(it.name));
  if (items.length === 0) {
    return { content: `empty directory: ${abs}`, attrs: { action: "ls", path: abs, path_kind: "directory", rows: "0", truncated: "false" } };
  }
  if (byTime) {
    const mt = (it) => (it.mtime === undefined ? -Infinity : it.mtime);
    items = items
      .map((it, i) => ({ it, i }))
      .sort((a, b) => {
        const d = mt(b.it) - mt(a.it);
        if (d !== 0) return d;
        return cmpBytes(a.it.name, b.it.name);
      })
      .map((x) => x.it);
  } else {
    items = items.slice().sort((a, b) => cmpBytes(a.name, b.name));
  }
  const lines = items.map((it) => fmtLine(it.name, it.dir, it.size, it.mtime));
  return { content: lines.join("\n"), attrs: { action: "ls", path: abs, path_kind: "directory", rows: String(lines.length), truncated: "false" } };
}

// ---- rg（对齐 vcore rg.go）----

const RG_DEFAULT_LIMIT = 100;

// rg [-i] [-l] [-m N] [-n] [-c] [-w] [--hidden] [-g GLOB]... <pattern> <path>
//   | rg --files [-g GLOB]... [--hidden] [path]
export async function cmdRg(fs, ctx, argv) {
  const pa = parseVCmdArgv(
    "rg",
    {
      bools: { "--files": true, "--hidden": true, "-i": true, "-l": true, "-n": true, "-c": true, "-w": true },
      values: { "-m": true },
      lists: { "-g": true },
      minPos: 0,
      maxPos: 2,
    },
    argv,
  );
  const globs = pa.lists["-g"] || [];
  for (const g of globs) {
    if (g.includes("!") || g.includes("**")) {
      throw execErr("rg", `glob "${g}" is not supported on this environment (restricted: no '!' negation or '**')`);
    }
  }

  const abs = pa.pos.length ? (fs._path ? await fs._path(pa.pos[pa.pos.length - 1], ctx) : pa.pos[pa.pos.length - 1]) : "";
  const hidden = pa.bools["--hidden"];

  if (pa.bools["--files"]) {
    if (pa.bools["-i"] || pa.bools["-l"] || pa.bools["-n"] || pa.bools["-c"] || pa.bools["-w"] || pa.values["-m"] !== undefined) {
      throw execErr("rg", "--files cannot be used with search flags (-i, -l, -m, -n, -c, -w)");
    }
    if (pa.pos.length > 1) throw execErr("rg", `unexpected argument "${pa.pos[1]}"`);
    const target = pa.pos[0] || defaultTarget(ctx);
    return rgFiles(fs, ctx, target, globs, hidden);
  }

  // 搜索模式：pattern + path
  if (pa.pos.length < 2) throw execErr("rg", "missing argument: rg [OPTIONS] <pattern> <path>");
  const pattern = pa.pos[0];
  if (RG_UNSUPPORTED_RE.test(pattern)) {
    throw execErr("rg", "pattern is not supported on this environment (restricted: no lookaround/backreference), use bash -c \"grep -P ...\" on a physical host");
  }
  let src = pattern;
  if (pa.bools["-w"]) src = `\\b(?:${src})\\b`;
  let re;
  try {
    re = new RegExp(src, pa.bools["-i"] ? "i" : "");
  } catch (e) {
    throw execErr("rg", `invalid pattern: ${e.message}`);
  }
  let maxPerFile = 0;
  if (pa.values["-m"] !== undefined) {
    const n = Number(pa.values["-m"]);
    if (!Number.isInteger(n) || n < 1) throw execErr("rg", `-m must be >= 1, got ${pa.values["-m"]}`);
    maxPerFile = n;
  }
  const target = pa.pos[1];
  const targetAbs = fs._path ? await fs._path(target, ctx) : target;
  const st = await fs.stat(target, ctx);
  if (st === null) throw execErr("rg", `${targetAbs}: no such file or directory`);

  let candidates = [];
  if (!st.dir) {
    candidates = [target];
  } else {
    candidates = await rgWalk(fs, ctx, target, globs, hidden);
  }
  return rgSearch(fs, ctx, targetAbs, pattern, candidates, re, maxPerFile, pa.bools["-l"], pa.bools["-c"]);
}

// rg 递归收集文件（对齐 Go rgWalk）：默认跳过隐藏（--hidden 收录）、
// RG_SKIP_DIRS（node_modules/vendor）恒跳过、glob 按文件名 OR 过滤。
async function rgWalk(fs, ctx, target, globs, hidden) {
  const w = await fs.walk(target, ctx);
  const out = [];
  for (const it of w.items || []) {
    if (it.dir) continue;
    const segs = it.path.split("/");
    // 任意路径段命中 skip 目录（node_modules/vendor）→ 恒跳过
    if (segs.some((s) => RG_SKIP_DIRS.has(s))) continue;
    const name = segs[segs.length - 1] || "";
    if (!hidden && isHidden(name)) continue;
    if (globs.length && !globs.some((g) => globMatch(g, name))) continue;
    out.push(it.path);
  }
  out.sort(cmpBytes);
  return out;
}

async function rgFiles(fs, ctx, target, globs, hidden) {
  const abs = fs._path ? await fs._path(target, ctx) : target;
  const st = await fs.stat(target, ctx);
  if (st === null) throw execErr("rg", `${abs}: no such file or directory`);
  let files = [];
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
  const rows = files.join("\n");
  return { content: rows, attrs: { action: "rg", path: abs, rows: String(files.length), truncated: String(truncated) } };
}

async function rgSearch(fs, ctx, abs, pattern, candidates, re, maxPerFile, filesOnly, countOnly) {
  const rows = [];
  let truncated = false;
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
    for (const m of ms) rows.push(`${m.path}:${m.line}:${m.text}`);
  }
  if (rows.length === 0) {
    return { content: `no matches for pattern "${pattern}" in ${abs}`, attrs: { action: "rg", path: abs, rows: "0", truncated: "false" } };
  }
  return { content: rows.join("\n"), attrs: { action: "rg", path: abs, rows: String(rows.length), truncated: String(truncated) } };
}

async function rgFile(fs, ctx, p, re, max) {
  const raw = await fs.readRaw(p, ctx);
  if (!raw || (raw.mime && raw.mime !== "text/plain")) return null; // 二进制跳过
  const text = String(raw.content ?? "");
  // 剥除尾随换行产生的空元素，避免 ^$ 等空串模式幻影报出文件末尾一行
  const lines = text.split("\n");
  if (lines.length && lines[lines.length - 1] === "") lines.pop();
  const out = [];
  for (let i = 0; i < lines.length; i++) {
    const line = lines[i].replace(/\r$/, ""); // CRLF 统一剥除
    if (re.test(line)) {
      out.push({ path: p, line: i + 1, text: line });
      if (out.length >= max) break;
    }
  }
  return out;
}

// ---- tree（对齐 vcore tree.go：JSON 输出，隐藏项完全跳过，大目录叶条目）----

const TREE_DEFAULT_DEPTH = 3;
const TREE_MAX_DEPTH = 5;
const TREE_MAX_NODES = 2000;

export async function cmdTree(fs, ctx, argv) {
  const pa = parseVCmdArgv("tree", { values: { "-L": true, "--depth": true }, minPos: 0, maxPos: 1 }, argv);
  let depth = TREE_DEFAULT_DEPTH;
  for (const flag of ["-L", "--depth"]) {
    const v = pa.values[flag];
    if (v !== undefined) {
      const n = Number(v);
      if (!Number.isInteger(n) || n < 1) throw execErr("tree", `${flag} must be >= 1, got ${v}`);
      depth = Math.min(n, TREE_MAX_DEPTH);
    }
  }
  const target = pa.pos[0] || defaultTarget(ctx);
  const abs = fs._path ? await fs._path(target, ctx) : target;
  const st = await fs.stat(target, ctx);
  if (st === null) throw execErr("tree", `${abs}: no such file or directory`);

  if (!st.dir) {
    const name = st.path.split("/").pop() || "";
    const entry = { name, dir: false, size: st.size ?? 0, mod_time: st.mtime === undefined ? 0 : Math.floor(st.mtime / 1000) };
    return { content: JSON.stringify(entry), attrs: { action: "tree", path: abs, rows: "1", truncated: "false" } };
  }

  const state = { count: 0, truncated: false };
  const items = await buildTreeDir(fs, ctx, target, depth, state);
  const out = { cwd: abs, dir: true, truncated: state.truncated, items };
  return { content: JSON.stringify(out), attrs: { action: "tree", path: abs, rows: String(state.count), truncated: String(state.truncated) } };
}

async function buildTreeDir(fs, ctx, dir, remain, state) {
  if (state.truncated) return [];
  const res = await fs.list(dir, ctx);
  const out = [];
  for (const e of res.items || []) {
    if (state.count >= TREE_MAX_NODES) {
      state.truncated = true;
      break;
    }
    const name = e.name;
    // 隐藏项（点开头）完全跳过：不显示不递归（对齐 GNU tree 默认，-a 才显示）
    if (isHidden(name)) continue;
    const size = e.dir ? 0 : (e.size ?? 0);
    const mt = e.mtime === undefined ? 0 : Math.floor(e.mtime / 1000);
    const ent = { name, dir: e.dir, size, mod_time: mt };
    state.count++;
    if (e.dir) {
      if (remain > 1 && !TREE_SKIP_DIRS.has(name)) {
        const sub = await buildTreeDir(fs, ctx, `${dir}/${name}`, remain - 1, state);
        ent.items = sub;
      }
    }
    out.push(ent);
  }
  out.sort((a, b) => cmpBytes(a.name, b.name));
  return out;
}

// ---- rm（对齐 vcore fileops.go rm 语义）----

// rm [-r] <path>：文件/无子项直接删；非空目录需 -r（附 (N items)）；不存在报错。
export async function cmdRm(fs, ctx, argv) {
  const pa = parseVCmdArgv("rm", { bools: { "-r": true }, minPos: 1, maxPos: 1 }, argv);
  const target = pa.pos[0];
  const abs = fs._path ? await fs._path(target, ctx) : target;
  const st = await fs.stat(target, ctx);
  if (st === null) throw execErr("rm", `${abs}: no such file or directory`);
  let out;
  if (!st.dir) {
    out = await fs.remove(target, ctx);
  } else {
    // 目录：需有子项判定（PageFS.remove 对空目录与不存在均报 no such file）
    const res = await fs.list(target, ctx);
    const children = (res.items || []).length;
    if (children > 0 && !pa.bools["-r"]) {
      throw execErr("rm", `${abs} is a non-empty directory (use -r)`);
    }
    out = await fs.remove(target, { ...ctx, recursive: pa.bools["-r"] });
  }
  return {
    content: out.items ? `removed ${out.removed} (${out.items} items)` : `removed ${out.removed}`,
    attrs: { action: "rm", path: out.removed },
  };
}

// ---- curl（对齐 vcore curl.go：流式下载、超限中止、二进制保留）----

// curl [-L] -o <path> <url> [--max-size <MB>]：仅 http/https；目标已存在报错不覆盖；
// ReadableStream 边读边计数，超限即中止（未写盘无半成品，对齐 Go io.Copy+LimitReader
// 语义）；落盘用 Blob 保留原始字节（二进制安全）。fetch 受页面 CORS 限制——失败按执行
// 错误返回；无 ReadableStream 的极简运行时回退整读但仍做字节计数。
export async function cmdCurl(fs, ctx, argv) {
  const pa = parseVCmdArgv("curl", { bools: { "-L": true }, values: { "-o": true, "--max-size": true }, minPos: 1, maxPos: 1 }, argv);
  const dst = pa.values["-o"];
  if (dst === undefined) throw execErr("curl", "-o is required");
  const rawurl = pa.pos[0];
  const m = rawurl.match(/^([a-zA-Z][a-zA-Z0-9+.-]*):\/\//);
  const scheme = m ? m[1].toLowerCase() : "";
  if (scheme !== "http" && scheme !== "https") {
    if (!scheme) throw execErr("curl", `invalid url "${rawurl}": missing scheme`);
    throw execErr("curl", `scheme not yet supported: ${scheme}`);
  }
  let maxSizeMB = 1024;
  if (pa.values["--max-size"] !== undefined) {
    const n = Number(pa.values["--max-size"]);
    if (!Number.isInteger(n) || n < 1) throw execErr("curl", `--max-size must be >= 1, got ${pa.values["--max-size"]}`);
    maxSizeMB = Math.min(n, 10240);
  }
  const abs = fs._path ? await fs._path(dst, ctx) : dst;
  const existing = await fs.stat(dst, ctx);
  if (existing !== null) throw execErr("curl", `destination ${abs} already exists`);
  let resp;
  try {
    resp = await fetch(rawurl, { redirect: pa.bools["-L"] ? "follow" : "error" });
  } catch (e) {
    throw execErr("curl", `fetch ${rawurl}: ${e?.message || e}`);
  }
  if (!resp.ok) throw execErr("curl", `fetch ${rawurl}: HTTP ${resp.status}`);
  const maxBytes = maxSizeMB << 20;
  const chunks = [];
  let total = 0;
  try {
    // 流式读取：边读边计数，超限中止——未写盘，天然无半成品
    const reader = resp.body && typeof resp.body.getReader === "function" ? resp.body.getReader() : null;
    if (reader) {
      for (;;) {
        const { done, value } = await reader.read();
        if (done) break;
        total += value.byteLength;
        if (total > maxBytes) {
          await reader.cancel();
          throw execErr("curl", `size limit exceeded (${Math.floor(total >> 20)}MB > ${maxSizeMB}MB)`);
        }
        chunks.push(value);
      }
    } else {
      // 无 ReadableStream（极简运行时）：整读回退，仍做字节计数
      const text = await resp.text();
      total = byteLenOf(text);
      if (total > maxBytes) throw execErr("curl", `size limit exceeded (${Math.floor(total >> 20)}MB > ${maxSizeMB}MB)`);
      chunks.push(new TextEncoder().encode(text));
    }
  } catch (e) {
    // 超限（已是 curl 错误）直接上抛；传输错误统一包装
    if (e instanceof Error && e.message.startsWith("curl: ")) throw e;
    throw execErr("curl", `fetch ${rawurl}: ${e?.message || e}`);
  }
  const blob = new Blob(chunks, { type: resp.headers && typeof resp.headers.get === "function" ? resp.headers.get("content-type") || "" : "" });
  const w = await fs.writeBlob(dst, blob, ctx);
  return {
    content: `downloaded ${rawurl} to ${abs} (${w.size ?? total} bytes)`,
    attrs: { action: "curl", path: abs, bytes: String(w.size ?? total) },
  };
}

function byteLenOf(s) {
  return new TextEncoder().encode(s).length;
}

// ---- cp / mv（对齐 vcore fileops.go §5.4：目标已存在报错不覆盖；目录移入自身子路径报错）----

// 复制单个文件/整棵目录树到 dstAbs（PageFS 无原生 Rename/MkdirAll——writeBlob 自动建父路径）
async function copyNode(fs, ctx, srcAbs, dstAbs) {
  const st = await fs.stat(srcAbs, ctx);
  if (st === null) throw execErr("cp", `cannot stat source ${srcAbs}: no such file or directory`);
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

// cp [-r] <src> <dst>：目录需 -r；目标已存在报错（不覆盖）；dst 为 src 自身或子路径报错。
export async function cmdCp(fs, ctx, argv) {
  const pa = parseVCmdArgv("cp", { bools: { "-r": true }, minPos: 2, maxPos: 2 }, argv);
  const srcAbs = fs._path ? await fs._path(pa.pos[0], ctx) : pa.pos[0];
  const dstAbs = fs._path ? await fs._path(pa.pos[1], ctx) : pa.pos[1];
  if (srcAbs === dstAbs) throw execErr("cp", `${srcAbs} and ${dstAbs} are identical`);
  const st = await fs.stat(srcAbs, ctx);
  if (st === null) throw execErr("cp", `cannot stat source ${srcAbs}: no such file or directory`);
  if (st.dir) {
    if (!pa.bools["-r"]) throw execErr("cp", `${srcAbs} is a directory (use -r)`);
    if (dstAbs.startsWith(srcAbs + "/")) throw execErr("cp", `cannot copy directory ${srcAbs} into itself: ${dstAbs}`);
  }
  if ((await fs.stat(dstAbs, ctx)) !== null) throw execErr("cp", `destination ${dstAbs} already exists`);
  await copyNode(fs, ctx, srcAbs, dstAbs);
  return { content: `copied ${srcAbs} to ${dstAbs}`, attrs: { action: "cp", path: dstAbs, source_path: srcAbs } };
}

// mv <src> <dst>：src==dst 报错；目标已存在报错（不覆盖）；目录移入自身子路径报错。
// PageFS 无原生 Rename：复制（文件/目录树）→ 删除源。
export async function cmdMv(fs, ctx, argv) {
  const pa = parseVCmdArgv("mv", { minPos: 2, maxPos: 2 }, argv);
  const srcAbs = fs._path ? await fs._path(pa.pos[0], ctx) : pa.pos[0];
  const dstAbs = fs._path ? await fs._path(pa.pos[1], ctx) : pa.pos[1];
  if (srcAbs === dstAbs) throw execErr("mv", `${srcAbs} and ${dstAbs} are identical`);
  const st = await fs.stat(srcAbs, ctx);
  if (st === null) throw execErr("mv", `cannot stat source ${srcAbs}: no such file or directory`);
  if (st.dir && dstAbs.startsWith(srcAbs + "/")) throw execErr("mv", `cannot move directory ${srcAbs} into itself: ${dstAbs}`);
  if ((await fs.stat(dstAbs, ctx)) !== null) throw execErr("mv", `destination ${dstAbs} already exists`);
  await copyNode(fs, ctx, srcAbs, dstAbs);
  try {
    await fs.remove(srcAbs, { ...ctx, recursive: true });
  } catch (e) {
    throw execErr("mv", `cannot remove source ${srcAbs} after copy: ${e?.message || e}`);
  }
  return { content: `moved ${srcAbs} to ${dstAbs}`, attrs: { action: "mv", path: dstAbs, source_path: srcAbs } };
}

// ---- 入口 ----

// runVCmd(action, argv, fs, ctx) → {content, attrs}；错误 throw Error("{cmd}: {原因}")。
// ctx: {workdir?}。fs: PageFS 接口对象（get/put/ls/rm/exists）。
export async function runVCmd(action, argv, fs, ctx = {}) {
  switch (action) {
    case "ls":
      return cmdLs(fs, ctx, argv);
    case "rg":
      return cmdRg(fs, ctx, argv);
    case "tree":
      return cmdTree(fs, ctx, argv);
    case "rm":
      return cmdRm(fs, ctx, argv);
    case "curl":
      return cmdCurl(fs, ctx, argv);
    case "cp":
      return cmdCp(fs, ctx, argv);
    case "mv":
      return cmdMv(fs, ctx, argv);
  }
  throw execErr(action, `unknown builtin (supported: ls, rg, tree, rm, curl, cp, mv)`);
}
