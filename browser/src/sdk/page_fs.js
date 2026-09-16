// PageFS — 浏览器本地文件系统（OPFS 实现）：Origin Private File System 真实目录树，
// fs 指令集 8 action（read/write/edit 本类实现，ls/rg/cp/mv/rm 委托 fsops.js）。
//
// 双端复用（同一套代码逻辑，逐字节同步，禁止漂移）：
//   - aic/ui/assets/libs/page_fs.js   — page 端（1host="page"，页面 OPFS）
//   - aic-pod/browser/src/sdk/page_fs.js — 浏览器扩展端（1host=host_id，扩展 OPFS）
// 两处 origin 不同，OPFS 物理隔离；代码必须保持一致，改动双向同步。
//
// 存储模型（v0.16 单根，与 cloud 的 $SESSION/$USER 资源隔离完全解耦）：
//   - navigator.storage.getDirectory() 为根，per-origin 私有文件系统；
//   - 真实目录树：目录由 getDirectoryHandle/entries() 枚举（O(子项)，无全库扫描）；
//   - 文件内容为字节（文本按 UTF-8 存），mtime = File.lastModified（目录无元数据）；
//   - 目录真实存在：空目录可 list/rm；mv/rm -r 用基元组合（读→写→删 / 递归枚举删除），
//     不依赖 FileSystemHandle.move() / removeEntry(recursive)（跨浏览器版本黑洞）；
//   - 平台不支持（Safari <15.2 / Firefox <111 / Chrome <86、Safari 私有浏览模式等）：
//     每次操作统一报错 fs {action}: OPFS not available in this browser（无回退）。
//   - 总量受浏览器 storage quota 管理，写失败（quota exceeded）按执行错误返回。
//
// 路径翻译（仅防呆，非隔离）：AI 经 fs 通道发来的路径可能带云语义
// 容器前缀（$SESSION/$USER/$AGENT、/sessions/{x}/、/home/{x}/）——resolve 一律剥离
// 映射到本地根下（如 $SESSION/foo → /foo）。
//
// 行为对齐 vcore（§2.6 三端一致）：offset/limit 1 基、行号前缀、128KB 预算、
// 二进制分支（page 无 UFS 概念 → 图片走 image_data data URI，超 600KB 阶梯压缩）、
// edit 唯一匹配/重叠校验。错误文案与 vcore 保持一致。
// 实现取舍：>8MB 大文件不整读是 Go 端的内存优化，输出语义相同；
// 浏览器端 Blob.text() 整读，输出与流式分支逐字节一致。
// ls 的 git 基本探测（§4.5）：目录下 .git 为目录 → is_repo=true + branch（读 .git/HEAD），
// 仅徽标用途无 git 语义（git 操作仅 cloud）。

import { pageExecutionPolicy } from "./execution_policy.js";
import { runFsOps } from "./fsops.js";

const MAX_CONTENT_BYTES = 128 << 10; // 128KB（§2.5，三端一致）
const IMAGE_DATA_MAX_BYTES = 600 * 1024; // image_data 投递标准（§2.2，三端一致）

// fs JSON 参数的合法字段（§2.1）：执行层对未知字段**宽忽略不拒绝**——AI 按 schema
// 全量传参是常态（如 write 时仍带 rg 的 context），多传参数当看不见。
// 本表仅作文档与排查参考，实际执行不因未知字段中断。
const ALLOWED_FIELDS = new Set([
  "msg_id",
  "action",
  "path",
  "offset",
  "limit",
  "content",
  "edits",
  "depth",
  "all",
  "pattern",
  "glob",
  "src",
  "dst",
  "recursive",
]);

// ---- 路径运算（镜像 proto.ResolvePath + WithinRoots，page 无盘符路径）----

function cleanPath(p) {
  const isAbs = p.startsWith("/");
  const parts = [];
  for (const seg of p.split("/")) {
    if (!seg || seg === ".") continue;
    if (seg === "..") {
      if (parts.length && parts[parts.length - 1] !== "..") parts.pop();
      else if (!isAbs) parts.push("..");
    } else {
      parts.push(seg);
    }
  }
  const out = (isAbs ? "/" : "") + parts.join("/");
  return out || (isAbs ? "/" : ".");
}

// resolvePath 本地单根解析：剥离云语义容器前缀（$SESSION/$USER/$AGENT、
// /sessions/{x}/、/home/{x}/ → 本地根下），绝对路径忽略 workdir，
// 相对路径基于 workdir（缺省 /）。无任何 $ 变量概念。
function resolvePath(p, workdir) {
  if (!p) throw new Error("proto: path is empty");
  let s = String(p);
  // 云语义容器前缀剥离（防呆，非隔离）：AI 经 exec 发 $SESSION/foo → /foo
  s = s.replace(/^\$(?:SESSION|USER|AGENT)(?:\/|$)/, "/");
  s = s.replace(/^\/(?:sessions|home)\/[^/]+(?:\/|$)/, "/");
  if (s.startsWith("/")) return cleanPath(s);
  if (!workdir) throw new Error(`proto: relative path "${p}" requires workdir`);
  if (!workdir.startsWith("/")) {
    throw new Error(`proto: workdir must be absolute, got "${workdir}"`);
  }
  return cleanPath(workdir + "/" + s);
}

// ---- 文本/MIME 判定（对齐 vcore result.go 的关键分支）----

function isTextBytes(bytes) {
  if (!bytes.length) return true;
  try {
    new TextDecoder("utf-8", { fatal: true }).decode(bytes);
    return true;
  } catch (_) {
    return false;
  }
}

const MAGIC_TABLE = [
  [[0x89, 0x50, 0x4e, 0x47], "image/png"],
  [[0xff, 0xd8, 0xff], "image/jpeg"],
  [[0x47, 0x49, 0x46, 0x38], "image/gif"],
  [[0x25, 0x50, 0x44, 0x46], "application/pdf"],
  [[0x50, 0x4b, 0x03, 0x04], "application/zip"],
  [[0x1f, 0x8b], "application/gzip"],
];

const EXT_MIME = {
  ".png": "image/png",
  ".jpg": "image/jpeg",
  ".jpeg": "image/jpeg",
  ".gif": "image/gif",
  ".webp": "image/webp",
  ".svg": "image/svg+xml",
  ".pdf": "application/pdf",
  ".mp4": "video/mp4",
  ".mp3": "audio/mpeg",
  ".wav": "audio/wav",
  ".zip": "application/zip",
  ".tar": "application/gzip",
  ".gz": "application/gzip",
  ".tgz": "application/gzip",
};

// detectMIME：magic bytes 嗅探，octet-stream 按扩展名细化（对齐 vcore detectMIME）
function detectMIME(head, path) {
  let mime = "";
  for (const [magic, m] of MAGIC_TABLE) {
    if (magic.every((b, i) => head[i] === b)) {
      mime = m;
      break;
    }
  }
  if (
    !mime &&
    head.length >= 12 &&
    head[0] === 0x52 &&
    head[1] === 0x49 &&
    head[2] === 0x46 &&
    head[3] === 0x46 &&
    head[8] === 0x57 &&
    head[9] === 0x45 &&
    head[10] === 0x42 &&
    head[11] === 0x50
  ) {
    mime = "image/webp"; // RIFF....WEBP
  }
  if (!mime)
    mime = isTextBytes(head) ? "text/plain" : "application/octet-stream";
  if (mime === "application/octet-stream") {
    const ext = path.slice(path.lastIndexOf(".")).toLowerCase();
    if (EXT_MIME[ext]) mime = EXT_MIME[ext];
  }
  return mime;
}

const VIEWABLE_IMAGE_MIMES = new Set([
  "image/png",
  "image/jpeg",
  "image/gif",
  "image/webp",
]);

// truncateContent 按 §2.5 截断：字节上限、UTF-8 边界收刀、只保留完整行
function truncateContent(s, maxBytes) {
  const bytes = new TextEncoder().encode(s);
  if (bytes.length <= maxBytes) return [s, false];
  let cut = maxBytes;
  while (cut > 0 && (bytes[cut] & 0xc0) === 0x80) cut--; // 回退 continuation byte
  let out = new TextDecoder().decode(bytes.subarray(0, cut));
  const idx = out.lastIndexOf("\n");
  if (idx >= 0) out = out.slice(0, idx + 1);
  return [out, true];
}

// countLines：'\n' 数量 +（末尾无换行符 ? 1 : 0）；空内容为 0 行（§4.3）
function countLines(s) {
  if (!s) return 0;
  let n = 0;
  for (let i = 0; i < s.length; i++) if (s[i] === "\n") n++;
  return s.endsWith("\n") ? n : n + 1;
}

function byteLen(s) {
  return new TextEncoder().encode(s).length;
}

// cmpBytes 按 UTF-8 字节序比较两个字符串（对齐 vcore ls 排序，§5.4：
// 禁止 locale 相关排序；JS 默认比较是 UTF-16 码元序，非 ASCII 不一致）。
function cmpBytes(a, b) {
  const x = new TextEncoder().encode(a);
  const y = new TextEncoder().encode(b);
  for (let i = 0; i < Math.min(x.length, y.length); i++) {
    if (x[i] !== y[i]) return x[i] - y[i];
  }
  return x.length - y.length;
}

function fsErr(action, msg) {
  return new Error(`fs ${action}: ${msg}`);
}

// 检测 LLM 双重 JSON 编码（与 vcore write.go 对齐）：oldText 含字面 \uXXXX
// 转义且其 Unicode 解码形式恰好在文件中存在时，返回可操作提示（否则空串）。
const unicodeEscapeRe = /\\u[0-9a-fA-F]{4}/g;
function doubleEncodingHint(content, oldText) {
  if (!oldText.includes("\\u")) return "";
  const decoded = oldText.replace(unicodeEscapeRe, (m) =>
    String.fromCharCode(parseInt(m.slice(2), 16)),
  );
  if (decoded === oldText || !content.includes(decoded)) return "";
  return ' (hint: oldText contains literal "\\u003c"-style escapes from double JSON encoding; decoded form matches the file, resend with actual characters)';
}

// ---- 图片压缩阶梯（对齐 vcore image.go：原尺寸质量 80/60/40 → 0.5 倍逐级缩尺寸）----
//
// 环境适配：page（window）与扩展 service worker 双环境同一份代码——
// SW 无 document，canvas 一律走 OffscreenCanvas（Chrome page 同支持，输出一致）；
// base64 走 arrayBuffer 分块编码（btoa 在 window/worker/SW 均可用）。

function makeCanvas(w, h) {
  if (typeof OffscreenCanvas !== "undefined") return new OffscreenCanvas(w, h);
  const c = document.createElement("canvas");
  c.width = w;
  c.height = h;
  return c;
}

function canvasToJpegBlob(canvas, quality) {
  if (typeof canvas.convertToBlob === "function") {
    return canvas.convertToBlob({ type: "image/jpeg", quality });
  }
  return new Promise((resolve) =>
    canvas.toBlob(resolve, "image/jpeg", quality),
  );
}

async function compressImage(blob) {
  const bmp = await createImageBitmap(blob);
  let scale = 1;
  for (let i = 0; i < 6; i++) {
    const w = Math.max(1, Math.round(bmp.width * scale));
    const h = Math.max(1, Math.round(bmp.height * scale));
    for (const q of [80, 60, 40]) {
      const canvas = makeCanvas(w, h);
      const ctx = canvas.getContext("2d");
      ctx.fillStyle = "#ffffff"; // JPEG 无透明通道，先铺白底
      ctx.fillRect(0, 0, w, h);
      ctx.drawImage(bmp, 0, 0, w, h);
      const out = await canvasToJpegBlob(canvas, q / 100);
      if (out && out.size <= IMAGE_DATA_MAX_BYTES) {
        bmp.close?.();
        return { blob: out, width: w, height: h, quality: q };
      }
    }
    scale *= 0.5;
  }
  bmp.close?.();
  throw new Error(
    `image still exceeds ${IMAGE_DATA_MAX_BYTES} bytes after downscaling`,
  );
}

async function blobToBase64(blob) {
  const bytes = new Uint8Array(await blob.arrayBuffer());
  let bin = "";
  const CHUNK = 0x8000;
  for (let i = 0; i < bytes.length; i += CHUNK) {
    bin += String.fromCharCode.apply(null, bytes.subarray(i, i + CHUNK));
  }
  return btoa(bin);
}

async function imageDimensions(blob) {
  try {
    const bmp = await createImageBitmap(blob);
    const d = [bmp.width, bmp.height];
    bmp.close?.();
    return d;
  } catch (_) {
    return [0, 0];
  }
}

// ---- OPFS 存储层（唯一实现，无版本分支）----

// defaultRootProvider：页面/worker/SW 环境根句柄来源。
// 平台不支持（旧浏览器、Safari 私有浏览模式抛出、无 navigator.storage 等）→
// throw，由 _root 统一映射为 fs {action}: OPFS not available in this browser。
async function defaultRootProvider() {
  if (
    typeof navigator === "undefined" ||
    !navigator.storage ||
    typeof navigator.storage.getDirectory !== "function"
  ) {
    throw new Error("OPFS not available");
  }
  const root = await navigator.storage.getDirectory();
  if (!root) throw new Error("OPFS not available");
  return root;
}

// splitPath："/a/b/c" → ["a","b","c"]；根 "/" → []
function splitPath(abs) {
  return abs.split("/").filter(Boolean);
}

// resolveDir：沿 parts 逐级解析目录句柄（create 时自动建）。
// 中间段是文件抛 TypeMismatchError（映射 "not a directory"）。
async function resolveDir(root, parts, create) {
  let d = root;
  for (const seg of parts) {
    d = await d.getDirectoryHandle(seg, { create: !!create });
  }
  return d;
}

// fsPathErr：OPFS 异常 → fs 错误文案（kind="dir" = 路径段应为目录；
// kind="file" = 读文件处碰到目录）。NotFound 与 TypeMismatch 逐项映射，
// 其余按原始 message 包装（quota 等由存储层传递）。
function fsPathErr(action, abs, e, kind) {
  const name = e?.name;
  if (name === "NotFoundError")
    return fsErr(action, `${abs}: no such file or directory`);
  if (name === "TypeMismatchError")
    return fsErr(
      action,
      kind === "dir" ? `${abs}: not a directory` : `${abs}: is a directory`,
    );
  return fsErr(action, e?.message || String(e));
}

// ---- PageFS ----

export class PageFS {
  // 无参构造：单根存储（OPFS 根），不依赖用户/会话。
  // rootProvider 供测试注入（内存实现）；缺省 navigator.storage.getDirectory。
  constructor(rootProvider, policy = pageExecutionPolicy()) {
    this.policy = policy;
    this._rootProvider = rootProvider || defaultRootProvider;
    this._rootResult = null; // {root} | {err:true}，惰性且失败缓存（平台错误不恢复）
  }

  // _root(action)：获取 OPFS 根句柄；平台不可用报统一错误（每次携带当前 action 名）。
  _root(action) {
    if (!this._rootResult) {
      this._rootResult = this._rootProvider()
        .then((root) => ({ root }))
        .catch(() => ({ err: true }));
    }
    return this._rootResult.then((r) => {
      if (r.err)
        throw fsErr(action, "OPFS not available in this browser");
      return r.root;
    });
  }

  // _readFileBytes：整读文件为字节（文本/二进制统一；文本由调用方解码）。
  async _readFileBytes(abs, action) {
    this.policy.checkFs(abs, false);
    if (abs === "/") throw fsErr(action, `${abs}: is a directory`);
    const root = await this._root(action);
    const parts = splitPath(abs);
    const name = parts.pop();
    let d;
    try {
      d = await resolveDir(root, parts, false);
    } catch (e) {
      throw fsPathErr(action, abs, e, "dir");
    }
    let fh;
    try {
      fh = await d.getFileHandle(name);
    } catch (e) {
      throw fsPathErr(action, abs, e, "file");
    }
    const f = await fh.getFile();
    return new Uint8Array(await f.arrayBuffer());
  }

  // _writeFileBytes：整写文件字节（覆写，父目录自动创建）。
  async _writeFileBytes(abs, bytes, action) {
    this.policy.checkFs(abs, true);
    if (abs === "/") throw fsErr(action, `${abs}: is a directory`);
    const root = await this._root(action);
    const parts = splitPath(abs);
    const name = parts.pop();
    let d;
    try {
      d = await resolveDir(root, parts, true);
    } catch (e) {
      throw fsPathErr(action, abs, e, "dir");
    }
    let fh;
    try {
      fh = await d.getFileHandle(name, { create: true });
    } catch (e) {
      throw fsPathErr(action, abs, e, "file");
    }
    const w = await fh.createWritable();
    try {
      await w.write(bytes);
    } catch (e) {
      // 流式写失败（quota exceeded 等）：close 不再有意义，按执行错误返回
      try {
        await w.abort?.();
      } catch (_) {
        /* 尽力关闭 */
      }
      throw fsErr(action, e?.message || String(e));
    }
    try {
      await w.close(); // close 提交（原子可见；quota on commit 在此失败）
    } catch (e) {
      // close 阶段失败（如 quota on commit）同样按 fs 错误文案契约包装，
      // 不向主线程/worker 转发链路透出裸 DOMException
      throw fsErr(action, e?.message || String(e));
    }
  }

  // _env 构造路径环境：本地单根，workdir 恒 /（无 $ 变量、无会话绑定；
  // fs 参数无 workdir——路径一律绝对，相对路径由 resolvePath 落到 /）
  _env() {
    return { workdir: "/" };
  }

  // run 执行一条 fs 请求（§6.1 body = {msg_id, action, ...fs JSON 参数}）。
  // 8 action：read/write/edit 为本类方法；ls/rg/cp/mv/rm 委托 fsops.js
  // （PageFS 原语驱动，与 vcore 语义一致）。
  // 返回 {content, attrs}；错误 throw Error("fs ...")。
  async run(params, ctx = {}) {
    params = params || {};
    const action = String(params.action || "").toLowerCase();
    // 宽容未知字段（用户定：AI 按 schema 全量传参是常态，多余参数当看不见——忽略不拒绝）。
    // 已知但未用字段（如 write 收到 context）同样忽略；attrs 仅报一条汇总 warning 便于排查。
    if (!action)
      throw fsErr(
        "",
        "action is required (supported: read, write, edit, ls, rg, cp, mv, rm)",
      );
    // 平台可用性检查（不支持即统一报错，无回退）
    await this._root(action);

    const env = this._env();

    switch (action) {
      case "read":
        return this._read(params, env);
      case "write":
        return this._write(params, env);
      case "edit":
        return this._edit(params, env);
      case "ls":
      case "rg":
      case "cp":
      case "mv":
      case "rm":
        return runFsOps(this, params, { workdir: env.workdir });
    }
    throw fsErr(
      "",
      `unknown action "${action}" (supported: read, write, edit, ls, rg, cp, mv, rm)`,
    );
  }

  // _resolve 路径规范化（本地单根，resolvePath 内剥离云语义容器前缀）
  _resolve(rawPath, env, action) {
    return resolvePath(String(rawPath || ""), env.workdir);
  }

  // ---- read（§4.2）----

  async _read(p, env) {
    if (!p.path) throw fsErr("read", "path is required");
    let offset = 1,
      limit = 1000;
    if (p.offset !== undefined && p.offset !== null) {
      offset = Number(p.offset);
      if (!Number.isInteger(offset) || offset < 1) {
        throw fsErr("read", `offset must be >= 1, got ${p.offset}`);
      }
    }
    if (p.limit !== undefined && p.limit !== null) {
      limit = Number(p.limit);
      if (!Number.isInteger(limit) || limit < 1) {
        throw fsErr("read", `limit must be >= 1, got ${p.limit}`);
      }
      if (limit > 1000) limit = 1000;
    }
    const abs = this._resolve(p.path, env, "read");
    const bytes = await this._readFileBytes(abs, "read");

    if (!isTextBytes(bytes)) return this._binaryResult(abs, bytes);

    const text = new TextDecoder().decode(bytes);
    const lines = text.split("\n");
    if (lines.length && lines[lines.length - 1] === "") lines.pop();
    const total = lines.length;
    if (offset > total)
      throw fsErr("read", `offset ${offset} exceeds ${total} lines`);
    let end = Math.min(offset - 1 + limit, total);
    let truncated = end < total;

    let body = "";
    for (let i = offset - 1; i < end; i++) body += `${i + 1}\t${lines[i]}\n`;
    // 128KB 内容上限先于 limit 触发：只保留完整行，rows/range 同步收紧（§4.2）
    const [cut, wasCut] = truncateContent(body, MAX_CONTENT_BYTES);
    if (wasCut) {
      body = cut;
      let rows = 0;
      for (const ch of body) if (ch === "\n") rows++;
      end = offset - 1 + rows;
      truncated = true;
    }

    const attrs = {
      action: "read",
      path: abs,
      mime: "text/plain",
      total_lines: String(total),
      rows: String(end - (offset - 1)),
      range: `${offset}-${end}`,
      truncated: String(truncated),
    };
    if (truncated)
      attrs.hint = `file has ${total} lines; this call returned ${offset}-${end}; pass offset=${end + 1} to continue reading`;
    return { content: body, attrs };
  }

  // 二进制分支（§4.2）：mime + size；可展示图片走 image_data（page 无 UFS，§2.2）
  async _binaryResult(abs, bytes) {
    const mime = detectMIME(bytes.subarray(0, 512), abs);
    const attrs = {
      action: "read",
      path: abs,
      mime,
      size: String(bytes.length),
    };
    if (!VIEWABLE_IMAGE_MIMES.has(mime)) {
      return {
        content: `Binary file: ${abs} (${mime}, ${bytes.length} bytes)`,
        attrs,
      };
    }
    let blob = new Blob([bytes], { type: mime });
    const [w, h] = await imageDimensions(blob);
    let outMime = mime;
    if (bytes.length > IMAGE_DATA_MAX_BYTES) {
      let c;
      try {
        c = await compressImage(blob);
      } catch (_) {
        throw fsErr(
          "read",
          `image too large even after compression (${bytes.length} bytes)`,
        );
      }
      attrs.image_compressed = `${bytes.length} bytes → image/jpeg ${c.width}x${c.height} quality ${c.quality} (${c.blob.size} bytes)`;
      blob = c.blob;
      outMime = "image/jpeg";
    }
    attrs.image_data = `data:${outMime};base64,${await blobToBase64(blob)}`;
    const dim = w > 0 ? `, ${w}x${h}` : "";
    return {
      content: `Image file: ${abs} (${mime}${dim}, ${bytes.length} bytes)`,
      attrs,
    };
  }

  // ---- write（§4.3）：content 必填，整文件覆写 ----

  async _write(p, env) {
    if (!p.path) throw fsErr("write", "path is required");
    if (p.content === undefined || p.content === null) {
      throw fsErr("write", "content is required");
    }
    const content = String(p.content);
    const abs = this._resolve(p.path, env, "write");
    const lines = countLines(content);
    const bytes = byteLen(content);
    await this._writeFileBytes(abs, new TextEncoder().encode(content), "write");
    return {
      content: `wrote file: ${abs} (${lines} lines, ${bytes} bytes)`,
      attrs: {
        action: "write",
        path: abs,
        mode: "overwrite",
        lines: String(lines),
        bytes: String(bytes),
      },
    };
  }

  // ---- edit（§4.4）：逐个顺序应用，部分成功语义（与 vcore 对齐）----

  async _edit(p, env) {
    if (!p.path) throw fsErr("edit", "path is required");
    const edits = Array.isArray(p.edits) ? p.edits : [];
    if (!edits.length) throw fsErr("edit", "edits is required");
    const abs = this._resolve(p.path, env, "edit");
    let bytes = await this._readFileBytes(abs, "edit");
    if (!isTextBytes(bytes)) throw fsErr("edit", `${abs} is not a text file`);
    let content = new TextDecoder().decode(bytes);

    // 逐个顺序应用：后一个 edit 匹配前一个应用后的内容；
    // 失败条目记录 edit[i]: 原因，不阻塞其余 edit（部分成功语义）。
    let applied = 0;
    const failed = [];
    for (let i = 0; i < edits.length; i++) {
      const e = edits[i];
      const idx = `edit[${i + 1}]`;
      if (!e || !e.oldText) {
        failed.push(`${idx}: oldText is required`);
        continue;
      }
      if (e.newText === e.oldText) {
        failed.push(`${idx}: newText must be different from oldText`);
        continue;
      }
      const old = e.oldText;
      const first = content.indexOf(old);
      if (first < 0) {
        failed.push(
          `${idx}: oldText not found in file${doubleEncodingHint(content, old)}`,
        );
        continue;
      }
      if (content.indexOf(old, first + old.length) >= 0) {
        const n = content.split(old).length - 1;
        failed.push(
          `${idx}: oldText matches ${n} locations; provide more surrounding context to make it unique`,
        );
        continue;
      }
      content =
        content.slice(0, first) + e.newText + content.slice(first + old.length);
      applied++;
    }
    if (applied === 0) {
      if (failed.length === 1) throw fsErr("edit", failed[0]);
      throw fsErr("edit", `no edits applied: ${failed.join("; ")}`);
    }
    await this._writeFileBytes(abs, new TextEncoder().encode(content), "edit");
    const attrs = { action: "edit", path: abs, edits: String(applied) };
    if (failed.length) {
      return {
        content: `updated file: ${abs} (${applied}/${edits.length} edits applied; failed: ${failed.join("; ")})`,
        attrs: { ...attrs, edits_failed: String(failed.length) },
      };
    }
    return {
      content: `updated file: ${abs} (${edits.length} edits)`,
      attrs,
    };
  }

  // ---- 前端操作对象接口（get/put/ls/rm/mkdir/mv/home/search/resolve）----
  // 面向前端程序的操作对象接口（$fs / 扩展端调用方直接使用，page_exec 不再包装）。
  // 底层方法 readRaw/writeBlob/writeText/list/stat/walk/remove/has 保留
  // （fsops 等内部使用，与 cloud_fs 无对应关系）。
  //
  // 与 cloud_fs 的文档化差异：
  //   - resolve() 返回规范化绝对路径（OPFS 无 URL 概念；cloud_fs 返回
  //     完整 URL 供 <img> 直链——page 端取图用 get() 拿 Blob 转 dataURL/objectURL）
  //   - rm() 递归删除（对齐 httpfs RemoveAll / cloud_fs.rm 语义，无需 recursive）

  // get 整读：文件 {ok, content: string|Blob, mime, size, path}；
  // 目录 {ok, dir:true, path, items}；均不存在抛错（与 cloud_fs 一致）。
  async get(path, ctx = {}) {
    const abs = this._path(path, ctx);
    if (abs === "/") {
      const l = await this.list(abs, ctx);
      return { ok: true, dir: true, path: l.path, items: l.items };
    }
    const st = await this.stat(abs, ctx);
    if (st === null) throw fsErr("get", `${abs}: no such file or directory`);
    if (st.dir) {
      const l = await this.list(abs, ctx);
      return { ok: true, dir: true, path: l.path, items: l.items };
    }
    const raw = await this.readRaw(abs, ctx);
    return { ok: true, ...raw };
  }

  // put 整写（覆写，父目录自动创建）：string 文本 / Blob 二进制通吃；
  // ArrayBuffer/TypedArray 归一为 Blob 走字节写入（2026-09-12 修：此前
  // 落入 String() 分支字节损坏）。
  // 返回 {ok, path, bytes}（与 cloud_fs.put 一致）。
  async put(path, content, ctx = {}) {
    if (content instanceof ArrayBuffer || ArrayBuffer.isView(content)) {
      content = new Blob([content]);
    }
    if (content instanceof Blob) {
      const r = await this.writeBlob(path, content, ctx);
      return { ok: true, path: r.path, bytes: r.size };
    }
    const r = await this.run(
      { action: "write", path, content: String(content) },
      ctx,
    );
    return {
      ok: true,
      path: r.attrs?.path || path,
      bytes: Number(r.attrs?.bytes ?? 0),
    };
  }

  // ls 一层目录列表（= list，cloud_fs 同名）。返回 {ok, path, is_repo?, branch?, items}。
  // opts.depth > 1 时递归展开子目录（items[].items 嵌套，与 cloud/host 对齐）。
  // git 基本探测（§4.5）：目录下 .git 为目录 → is_repo=true + branch（读 .git/HEAD）；
  // 仅徽标用途，无 git 语义（git 操作仅 cloud）。
  async ls(path, ctx = {}) {
    const r = await this.list(path, ctx);
    const items = await this._markRepos(r.items, ctx);
    if ((ctx.depth || 1) > 1) {
      for (const it of items) {
        if (it.dir) it.items = await this._markRepos((await this.list(it.path, ctx)).items, ctx);
      }
    }
    const self = await this._gitRepoInfo(r.path || this._path(path, ctx), ctx);
    return {
      ok: true,
      path: r.path,
      ...(self.isRepo ? { is_repo: true } : {}),
      ...(self.branch ? { branch: self.branch } : {}),
      items,
    };
  }

  // _gitRepoInfo(dirAbs, ctx)：.git 为目录 → {isRepo:true, branch}（读 .git/HEAD 的
  // "ref: refs/heads/<name>" 行；detached/读取失败 → branch:""）。与 vcore gitRepoInfo /
  // vigo httpfs isGitRepo 同判定（.git 文件形态的 worktree/submodule 不识别）。
  async _gitRepoInfo(dirAbs, ctx = {}) {
    const base = String(dirAbs || "").replace(/\/+$/, "");
    let st = null;
    try {
      st = await this.stat(`${base}/.git`, ctx);
    } catch (e) {
      st = null; // 适配器对不存在路径抛错（PageFS 本体返回 null）
    }
    if (!st || !st.dir) return { isRepo: false, branch: "" };
    let head = "";
    try {
      const raw = await this.readRaw(`${base}/.git/HEAD`, ctx);
      if (raw && typeof raw.content === "string") head = raw.content;
    } catch (e) {
      /* .git 存在但 HEAD 不可读：仓库成立、分支未知 */
    }
    const line = head.split("\n")[0].trim();
    const prefix = "ref: refs/heads/";
    return { isRepo: true, branch: line.startsWith(prefix) ? line.slice(prefix.length) : "" };
  }

  // _markRepos(items, ctx)：为目录条目补 is_repo/branch（原对象上新增字段，返回同一数组）。
  async _markRepos(items, ctx = {}) {
    for (const it of items || []) {
      if (!it.dir) continue;
      const repo = await this._gitRepoInfo(it.path, ctx);
      if (repo.isRepo) it.is_repo = true;
      if (repo.branch) it.branch = repo.branch;
    }
    return items || [];
  }

  // rm 递归删除（对齐 httpfs RemoveAll / cloud_fs.rm）：文件或整棵目录树。
  // 返回 {ok, path, removed}（与 cloud_fs.rm 一致）。
  async rm(path, ctx = {}) {
    const r = await this.remove(path, { ...ctx, recursive: true });
    return { ok: true, path: this._path(path, ctx), removed: r.removed };
  }

  // mkdir 显式建目录（OPFS 真实目录，父目录自动创建；已存在幂等）。
  async mkdir(path, ctx = {}) {
    this.policy.checkFs(this._path(path, ctx), true);
    const abs = this._path(path, ctx);
    const root = await this._root("mkdir");
    const parts = splitPath(abs);
    let d;
    try {
      d = await resolveDir(root, parts, true);
    } catch (e) {
      throw fsPathErr("mkdir", abs, e, "dir");
    }
    return { ok: true, path: abs };
  }

  // mv 移动：委托 fs 指令 mv 语义（src/dst 校验、目标已存在报错、目录递归移动）。
  async mv(src, dst, ctx = {}) {
    const r = await this.run({ action: "mv", src, dst }, ctx);
    return { ok: true, path: this._path(dst, ctx) };
  }

  // home 本地根恒为 /
  home() {
    return "/";
  }

  // Recursive filename-only search; directory names and content never match.
  async search(path, opts = {}, ctx = {}) {
    const {searchOptions, filenameMatches} = await import('./file_search.js');
    const {glob, limit, depth: maxDepth} = searchOptions(opts);
    const abs = this._path(path, ctx);
    const rows = [];
    const visit = async (dir, depth) => {
      const listing = await this.list(dir, ctx);
      for (const it of listing.items) {
        if (rows.length >= limit) return;
        const name = it.path.split('/').pop();
        if (name.startsWith('.')) continue;
        if (it.dir) {
          if (!maxDepth || depth < maxDepth) await visit(it.path, depth + 1);
        } else if (filenameMatches(name, glob)) rows.push(it);
      }
    };
    await visit(abs, 1);
    return { ok: true, path: abs, rows };
  }

  // resolve 资源定位 URL：读内容 → Blob → objectURL（调用方负责 revoke）。
  async resolve(path, ctx = {}) {
    const r = await this.get(path, ctx);
    if (r.dir) throw new Error("page_fs.resolve: 目录无资源定位");
    const blob =
      r.content instanceof Blob
        ? r.content
        : new Blob([r.content || ""], { type: r.mime || "text/plain" });
    return URL.createObjectURL(blob);
  }

  // ---- 前端层底层方法（供内部实现与 AI 通道使用：
  //      不走 NATS 协议信封：文本原文 / Blob 直取 / 目录枚举）----

  // 规格化：确保以 "/" 开头（只判断加不加 "/"，不做 workdir/前缀映射）
  _path(rawPath, _ctx = {}) {
    const p = String(rawPath == null ? "" : rawPath);
    return p.startsWith("/") ? p : "/" + p;
  }

  // readRaw 前端友好读：文本返回原文 string，二进制返回 Blob（附 mime/size）
  async readRaw(path, ctx = {}) {
    const abs = this._path(path, ctx);
    const bytes = await this._readFileBytes(abs, "read");
    if (!isTextBytes(bytes)) {
      const mime = detectMIME(bytes.subarray(0, 512), abs);
      return {
        content: new Blob([bytes], { type: mime }),
        mime,
        size: bytes.length,
        path: abs,
      };
    }
    return {
      content: new TextDecoder().decode(bytes),
      mime: "text/plain",
      size: bytes.length,
      path: abs,
    };
  }

  // writeBlob 二进制写入（§2.2：browser 截图等本地产出物落 host fs，
  // agent 经 fs.read 读图；前端层底层方法，不走协议信封）。返回 {ok, path, size}。
  async writeBlob(path, blob, ctx = {}) {
    if (!(blob instanceof Blob))
      throw fsErr("write", "writeBlob: blob is required");
    const abs = this._path(path, ctx);
    const bytes = new Uint8Array(await blob.arrayBuffer());
    await this._writeFileBytes(abs, bytes, "write");
    return { ok: true, path: abs, size: blob.size };
  }

  // writeText 文本写入：内部走 write 语义，返回 {ok,path,size}。
  async writeText(path, text, ctx = {}) {
    const r = await this.run(
      { action: "write", path, content: String(text) },
      ctx,
    );
    return {
      ok: true,
      path: r.attrs?.path || path,
      size: Number(r.attrs?.bytes ?? 0),
    };
  }

  // list 目录列举：O(子项) 目录遍历（entries()）；文件项经 getFile() 取
  // size/lastModified（目录无元数据 → size undefined/mtime undefined）。
  // 目录不存在时返回空列表（与 $fs 前端接口的历史行为一致，存在性由 stat 判定）。
  async list(path, ctx = {}) {
    this.policy.checkFs(this._path(path, ctx), false);
    const abs = this._path(path, ctx);
    const root = await this._root("list");
    const parts = splitPath(abs);
    const prefix = abs.endsWith("/") ? abs : abs + "/";
    const items = [];
    let d;
    try {
      d = await resolveDir(root, parts, false);
    } catch (e) {
      if (e?.name === "NotFoundError") {
        return { ok: true, path: prefix, items };
      }
      throw fsPathErr("list", abs, e, "dir");
    }
    for await (const [name, handle] of d.entries()) {
      if (handle.kind === "directory") {
        items.push({
          name,
          path: prefix + name + "/",
          dir: true,
          size: undefined,
          mtime: undefined,
        });
      } else {
        const f = await handle.getFile();
        items.push({
          name,
          path: prefix + name,
          dir: false,
          size: f.size,
          mtime: f.lastModified,
        });
      }
    }
    // 排序对齐 vcore（§5.4）：UTF-8 字节序、目录不优先（目录名带 / 后缀自然参与排序）
    items.sort((a, b) => cmpBytes(a.name, b.name));
    return { ok: true, path: prefix, items };
  }

  // stat 单路径状态（fsops 用）：文件返回 {path,dir:false,size,mtime}；
  // 目录返回 {path,dir:true,size:0,mtime:undefined}（目录无元数据）；
  // 本地根 / 恒存在（有效目录）；均不存在返回 null
  // （与 vcore Stat 语义对齐：不存在报错由调用方处理）。
  async stat(path, ctx = {}) {
    this.policy.checkFs(this._path(path, ctx), false);
    const abs = this._path(path, ctx);
    if (abs === "/") {
      return { path: abs, dir: true, size: 0, mtime: undefined };
    }
    const root = await this._root("stat");
    const parts = splitPath(abs);
    const name = parts[parts.length - 1];
    let d;
    try {
      d = await resolveDir(root, parts.slice(0, -1), false);
    } catch (e) {
      if (e?.name === "NotFoundError") return null;
      throw fsPathErr("stat", abs, e, "dir");
    }
    try {
      await d.getDirectoryHandle(name);
      return {
        path: abs.endsWith("/") ? abs : abs + "/",
        dir: true,
        size: 0,
        mtime: undefined,
      };
    } catch (e) {
      if (e?.name !== "NotFoundError" && e?.name !== "TypeMismatchError")
        throw fsPathErr("stat", abs, e, "dir");
      // NotFound/TypeMismatch：无目录 → 试文件（TypeMismatch = 名是文件）
    }
    try {
      const fh = await d.getFileHandle(name);
      const f = await fh.getFile();
      return {
        path: abs,
        dir: false,
        size: f.size,
        mtime: f.lastModified,
      };
    } catch (e) {
      if (e?.name === "NotFoundError") return null;
      throw fsPathErr("stat", abs, e, "file");
    }
  }

  // walk 递归全量列举（fsops 的 rg/ls 用）：返回平铺 {path,dir,size,mtime}[]，
  // 目录真实存在（含空目录）；不做隐藏/skip 过滤——过滤是
  // 指令语义（fsops.js），fs 层只提供原始树。
  async walk(path, ctx = {}) {
    this.policy.checkFs(this._path(path, ctx), false);
    const abs = this._path(path, ctx);
    const root = await this._root("walk");
    const parts = splitPath(abs);
    const prefix = abs.endsWith("/") ? abs : abs + "/";
    const items = [];
    const rec = async (dir, curPrefix) => {
      for await (const [name, handle] of dir.entries()) {
        const p = curPrefix + name + (handle.kind === "directory" ? "/" : "");
        if (handle.kind === "directory") {
          items.push({ path: p, dir: true, size: 0, mtime: undefined });
          await rec(handle, p);
        } else {
          const f = await handle.getFile();
          items.push({
            path: p,
            dir: false,
            size: f.size,
            mtime: f.lastModified,
          });
        }
      }
    };
    let d;
    try {
      d = await resolveDir(root, parts, false);
    } catch (e) {
      if (e?.name === "NotFoundError") {
        return { ok: true, path: prefix, items };
      }
      throw fsPathErr("walk", abs, e, "dir");
    }
    await rec(d, prefix);
    return { ok: true, path: prefix, items };
  }

  // remove 删除（对齐 vcore rm 语义）：文件/空目录直接删；非空目录需
  // ctx.recursive（-r）否则报错；递归时先子后父枚举删除（不依赖
  // removeEntry(recursive) 参数，全基线 API）。items = 删除条目数（含目录，
  // 对齐 vcore countEntries）。本地根 / 禁止删除。
  async remove(path, ctx = {}) {
 if (ctx.recursive && (await this.stat(path, ctx))?.dir) {
 const tree = await this.walk(path, ctx);
 for (const item of tree.items || []) this.policy.checkFs(item.path, true);
 }
    this.policy.checkFs(this._path(path, ctx), true);
    const abs = this._path(path, ctx);
    if (abs === "/") throw fsErr("rm", "cannot remove root");
    const root = await this._root("rm");
    const parts = splitPath(abs);
    const name = parts[parts.length - 1];
    let d;
    try {
      d = await resolveDir(root, parts.slice(0, -1), false);
    } catch (e) {
      if (e?.name === "NotFoundError")
        throw fsErr("rm", `${abs}: no such file or directory`);
      throw fsErr("rm", e?.message || String(e));
    }

    // 目录分支：非空需 recursive；递归删除先子后父（不依赖 removeEntry(recursive)），
    // items = 删除条目数（含目录，对齐 vcore countEntries）。
    let dirHandle = null;
    try {
      dirHandle = await d.getDirectoryHandle(name);
    } catch (e) {
      if (e?.name !== "NotFoundError" && e?.name !== "TypeMismatchError")
        throw fsErr("rm", e?.message || String(e));
      // NotFound/TypeMismatch：非目录 → 试文件（TypeMismatch = 名是文件）
    }
    if (dirHandle) {
      let count = 0;
      const removeTree = async (dir) => {
        for await (const [n, h] of dir.entries()) {
          if (h.kind === "directory") {
            count++;
            await removeTree(h);
            await dir.removeEntry(n); // 子目录清空后删除自身
          } else {
            count++;
            await dir.removeEntry(n);
          }
        }
      };
      // 先判空：空目录直接删；非空且非 recursive 报错
      let first = await dirHandle.entries().next();
      let empty = first.done;
      if (!empty && !ctx.recursive) {
        throw fsErr("rm", `${abs} is a non-empty directory (use -r)`);
      }
      if (!empty) {
        await removeTree(dirHandle);
      }
      await d.removeEntry(name);
      const res = { ok: true, removed: abs };
      if (count > 0) res.items = count;
      return res;
    }

    // 文件分支
    try {
      await d.removeEntry(name);
    } catch (e) {
      if (e?.name === "NotFoundError")
        throw fsErr("rm", `${abs}: no such file or directory`);
      throw fsErr("rm", e?.message || String(e));
    }
    return { ok: true, removed: abs };
  }

  // has 存在性检查（boolean；原 exists 语义，fsops 等内部使用）。
  // 文件与目录均算存在（OPFS 真实目录树语义）。
  async has(path, ctx = {}) {
    const st = await this.stat(path, ctx);
    return st !== null;
  }
}
