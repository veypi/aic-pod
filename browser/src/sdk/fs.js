// PageFS — 浏览器端 fs 实现（§4.5）：IndexedDB 双根（$USER/$SESSION），read/write/edit。
//
// 双端复用（同一套代码逻辑，逐字节同步，禁止漂移）：
//   - aic/ui/assets/libs/page_fs.js   — page 端（1host="page"，页面 IndexedDB）
//   - aic-pod/browser/src/sdk/fs.js   — 浏览器扩展端（1host=host_id，扩展 IndexedDB）
// 两处 origin 不同，IndexedDB 物理隔离；代码必须保持一致，改动双向同步。
//
// 权限模型：$USER/$SESSION 仅作路径规范（key 寻址），不做根收容——
// 浏览器端文件权限是全部的（page/扩展一致），任意绝对路径（含 .. 折叠后
// 跳出变量根）均可读写；服务端权限门控（等级审批）不受影响。
//
// 存储模型（§4.5）：
//   - 每用户一个 IndexedDB 库（aic-page-fs-{uid}），按根分 store（user/session）；
//   - key = 解析后的完整绝对路径（/home/{uid}/... → user store；
//     /sessions/{sid}/... → session store），$SESSION 数据随 session 保留、不自动清除；
//   - 记录 = {c: string|Blob, m: mtime}；文本存 string，二进制存 Blob；
//   - IndexedDB 无目录概念，write 的"父目录自动创建"天然成立；
//   - 总量受浏览器 storage quota 管理，写失败（quota exceeded）按执行错误返回。
//
// 行为对齐 vcore（§2.6 三端一致）：offset/limit 1 基、行号前缀、512KB 预算、
// 二进制分支（page 无 UFS 概念 → 图片走 image_data data URI，超 600KB 阶梯压缩）、
// edit 唯一匹配/重叠校验。错误文案与 vcore 保持一致。
// 实现取舍：>8MB 大文件不整读是 Go 端的内存优化，输出语义相同；
// 浏览器端 Blob.text() 整读，输出与流式分支逐字节一致。

const MAX_CONTENT_BYTES = 512 << 10; // 512KB（§2.5，三端一致）
const IMAGE_DATA_MAX_BYTES = 600 * 1024; // image_data 投递标准（§2.2，三端一致）

// fs JSON 参数的合法字段（§2.1：未知字段报错）
const ALLOWED_FIELDS = new Set([
  "msg_id", "action", "path", "workdir", "offset", "limit", "content", "edits",
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

// resolvePath 按 §2.1.1 可解析层展开：$VAR 最长前缀匹配（后跟 / 或结束）、
// 未匹配 $ 按字面相对路径、绝对路径忽略 workdir。
// 注意：$USER/$SESSION 仅作路径规范（IndexedDB key 寻址），不做根收容——
// 浏览器端文件权限是全部的（page/扩展一致），.. 折叠后的任意绝对路径均可访问。
function resolvePath(p, workdir, vars) {
  if (!p) throw new Error("proto: path is empty");
  if (p[0] === "$") {
    let name = "", rest = "";
    for (const k of Object.keys(vars)) {
      if (k.length > name.length && p.startsWith(k) &&
        (p.length === k.length || p[k.length] === "/")) {
        name = k;
        rest = p.slice(k.length);
      }
    }
    if (name) {
      // $VAR 展开后允许 .. 跳出变量根（权限全部，仅路径规范化）
      return cleanPath(vars[name] + rest);
    }
  }
  if (p.startsWith("/")) return cleanPath(p);
  if (!workdir) throw new Error(`proto: relative path "${p}" requires workdir`);
  if (!workdir.startsWith("/")) {
    throw new Error(`proto: workdir must be absolute, got "${workdir}"`);
  }
  return cleanPath(workdir + "/" + p);
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
  ".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg",
  ".gif": "image/gif", ".webp": "image/webp", ".svg": "image/svg+xml",
  ".pdf": "application/pdf", ".mp4": "video/mp4", ".mp3": "audio/mpeg",
  ".wav": "audio/wav", ".zip": "application/zip", ".tar": "application/gzip",
  ".gz": "application/gzip", ".tgz": "application/gzip",
};

// detectMIME：magic bytes 嗅探，octet-stream 按扩展名细化（对齐 vcore detectMIME）
function detectMIME(head, path) {
  let mime = "";
  for (const [magic, m] of MAGIC_TABLE) {
    if (magic.every((b, i) => head[i] === b)) { mime = m; break; }
  }
  if (!mime && head.length >= 12 &&
    head[0] === 0x52 && head[1] === 0x49 && head[2] === 0x46 && head[3] === 0x46 &&
    head[8] === 0x57 && head[9] === 0x45 && head[10] === 0x42 && head[11] === 0x50) {
    mime = "image/webp"; // RIFF....WEBP
  }
  if (!mime) mime = isTextBytes(head) ? "text/plain" : "application/octet-stream";
  if (mime === "application/octet-stream") {
    const ext = path.slice(path.lastIndexOf(".")).toLowerCase();
    if (EXT_MIME[ext]) mime = EXT_MIME[ext];
  }
  return mime;
}

const VIEWABLE_IMAGE_MIMES = new Set(["image/png", "image/jpeg", "image/gif", "image/webp"]);

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
    String.fromCharCode(parseInt(m.slice(2), 16))
  );
  if (decoded === oldText || !content.includes(decoded)) return "";
  return ' (hint: oldText contains literal "\\u003c"-style escapes from double JSON encoding; decoded form matches the file, resend with actual characters)';
}

// ---- IndexedDB 最小封装 ----

function idbOpen(uid) {
  return new Promise((resolve, reject) => {
    const req = indexedDB.open(`aic-page-fs-${uid}`, 1);
    req.onupgradeneeded = () => {
      const db = req.result;
      if (!db.objectStoreNames.contains("user")) db.createObjectStore("user");
      if (!db.objectStoreNames.contains("session")) db.createObjectStore("session");
    };
    req.onsuccess = () => resolve(req.result);
    req.onerror = () => reject(req.error);
  });
}

function idbGet(db, store, key) {
  return new Promise((resolve, reject) => {
    const req = db.transaction(store, "readonly").objectStore(store).get(key);
    req.onsuccess = () => resolve(req.result);
    req.onerror = () => reject(req.error);
  });
}

function idbPut(db, store, key, value) {
  return new Promise((resolve, reject) => {
    const tx = db.transaction(store, "readwrite");
    tx.objectStore(store).put(value, key);
    tx.oncomplete = () => resolve();
    tx.onerror = () => reject(tx.error);
    tx.onabort = () => reject(tx.error);
  });
}

function idbAllKeys(db, store) {
  return new Promise((resolve, reject) => {
    const req = db.transaction(store, "readonly").objectStore(store).getAllKeys();
    req.onsuccess = () => resolve(req.result || []);
    req.onerror = () => reject(req.error);
  });
}

function idbDelete(db, store, key) {
  return new Promise((resolve, reject) => {
    const req = db.transaction(store, "readwrite").objectStore(store).delete(key);
    req.onsuccess = () => resolve();
    req.onerror = () => reject(req.error);
  });
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
  return new Promise((resolve) => canvas.toBlob(resolve, "image/jpeg", quality));
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
  throw new Error(`image still exceeds ${IMAGE_DATA_MAX_BYTES} bytes after downscaling`);
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

// ---- PageFS ----

export class PageFS {
  // opts.uid：当前用户 id（库命名空间 + $USER 根）
  constructor({ uid } = {}) {
    if (!uid) throw new Error("page_fs: uid is required");
    this._uid = uid;
    this._db = null; // 懒打开（首次 run）
  }

  async _ensureDB() {
    if (!this._db) this._db = await idbOpen(this._uid);
    return this._db;
  }

  // _env 构造路径环境：$USER/$SESSION 双根（仅路径规范，无权限收容），
  // workdir 缺省 $SESSION（§2.1.1）
  _env(sid, workdirRaw) {
    const vars = {
      $USER: `/home/${this._uid}`,
      $SESSION: `/sessions/${sid}`,
    };
    const workdir = workdirRaw
      ? resolvePath(String(workdirRaw), "", vars)
      : vars.$SESSION;
    return { vars, workdir };
  }

  // run 执行一条 fs 请求（§6.1 body = {msg_id, action, ...fs JSON 参数}）。
  // ctx.sessionId 绑定 $SESSION 根。返回 {content, attrs}；错误 throw Error("fs ...")。
  async run(params, ctx = {}) {
    params = params || {};
    const action = String(params.action || "").toLowerCase();
    for (const k of Object.keys(params)) {
      if (!ALLOWED_FIELDS.has(k)) throw fsErr(action, `unknown field "${k}"`);
    }
    if (!action) throw fsErr("", "action is required (supported: read, write, edit)");

    const env = this._env(ctx.sessionId || "", params.workdir);

    switch (action) {
      case "read": return this._read(params, env);
      case "write": return this._write(params, env);
      case "edit": return this._edit(params, env);
    }
    throw fsErr("", `unknown action "${action}" (supported: read, write, edit)`);
  }

  // _resolve 路径规范化（无根收容：$USER/$SESSION 仅规范路径，权限全部）
  _resolve(rawPath, env, action) {
    return resolvePath(String(rawPath || ""), env.workdir, env.vars);
  }

  _storeOf(abs) {
    // 边界对齐：/sessions 本身也归 session store（否则 ls /sessions 扫错库、恒空）
    return abs === "/sessions" || abs.startsWith("/sessions/") ? "session" : "user";
  }

  async _getFile(abs, action) {
    const db = await this._ensureDB();
    const rec = await idbGet(db, this._storeOf(abs), abs);
    if (rec === undefined) throw fsErr(action, `${abs}: no such file or directory`);
    return rec;
  }

  // ---- read（§4.2）----

  async _read(p, env) {
    if (!p.path) throw fsErr("read", "path is required");
    let offset = 1, limit = 1000;
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
    const rec = await this._getFile(abs, "read");
    const bytes = rec.c instanceof Blob
      ? new Uint8Array(await rec.c.arrayBuffer())
      : new TextEncoder().encode(rec.c);

    if (!isTextBytes(bytes)) return this._binaryResult(abs, bytes);

    const text = typeof rec.c === "string" ? rec.c : new TextDecoder().decode(bytes);
    const lines = text.split("\n");
    if (lines.length && lines[lines.length - 1] === "") lines.pop();
    const total = lines.length;
    if (offset > total) throw fsErr("read", `offset ${offset} exceeds ${total} lines`);
    let end = Math.min(offset - 1 + limit, total);
    let truncated = end < total;

    let body = "";
    for (let i = offset - 1; i < end; i++) body += `${i + 1}\t${lines[i]}\n`;
    // 512KB 内容上限先于 limit 触发：只保留完整行，rows/range 同步收紧（§4.2）
    const [cut, wasCut] = truncateContent(body, MAX_CONTENT_BYTES);
    if (wasCut) {
      body = cut;
      let rows = 0;
      for (const ch of body) if (ch === "\n") rows++;
      end = offset - 1 + rows;
      truncated = true;
    }

    return {
      content: body,
      attrs: {
        action: "read", path: abs, mime: "text/plain",
        total_lines: String(total),
        rows: String(end - (offset - 1)),
        range: `${offset}-${end}`,
        truncated: String(truncated),
      },
    };
  }

  // 二进制分支（§4.2）：mime + size；可展示图片走 image_data（page 无 UFS，§2.2）
  async _binaryResult(abs, bytes) {
    const mime = detectMIME(bytes.subarray(0, 512), abs);
    const attrs = { action: "read", path: abs, mime, size: String(bytes.length) };
    if (!VIEWABLE_IMAGE_MIMES.has(mime)) {
      return { content: `Binary file: ${abs} (${mime}, ${bytes.length} bytes)`, attrs };
    }
    let blob = new Blob([bytes], { type: mime });
    const [w, h] = await imageDimensions(blob);
    let outMime = mime;
    if (bytes.length > IMAGE_DATA_MAX_BYTES) {
      let c;
      try {
        c = await compressImage(blob);
      } catch (_) {
        throw fsErr("read", `image too large even after compression (${bytes.length} bytes)`);
      }
      attrs.image_compressed =
        `${bytes.length} bytes → image/jpeg ${c.width}x${c.height} quality ${c.quality} (${c.blob.size} bytes)`;
      blob = c.blob;
      outMime = "image/jpeg";
    }
    attrs.image_data = `data:${outMime};base64,${await blobToBase64(blob)}`;
    const dim = w > 0 ? `, ${w}x${h}` : "";
    return { content: `Image file: ${abs} (${mime}${dim}, ${bytes.length} bytes)`, attrs };
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
    const db = await this._ensureDB();
    try {
      // IndexedDB 无目录概念，"父目录不存在自动创建"天然成立
      await idbPut(db, this._storeOf(abs), abs, { c: content, m: Date.now() });
    } catch (e) {
      // quota exceeded 等存储层失败按执行错误返回（§4.5）
      throw fsErr("write", e?.message || String(e));
    }
    return {
      content: `wrote file: ${abs} (${lines} lines, ${bytes} bytes)`,
      attrs: {
        action: "write", path: abs, mode: "overwrite",
        lines: String(lines), bytes: String(bytes),
      },
    };
  }

  // ---- edit（§4.4）：逐个顺序应用，部分成功语义（与 vcore 对齐）----

  async _edit(p, env) {
    if (!p.path) throw fsErr("edit", "path is required");
    const edits = Array.isArray(p.edits) ? p.edits : [];
    if (!edits.length) throw fsErr("edit", "edits is required");
    const abs = this._resolve(p.path, env, "edit");
    const rec = await this._getFile(abs, "edit");
    if (rec.c instanceof Blob) {
      const bytes = new Uint8Array(await rec.c.arrayBuffer());
      if (!isTextBytes(bytes)) throw fsErr("edit", `${abs} is not a text file`);
    }
    let content = typeof rec.c === "string"
      ? rec.c
      : new TextDecoder().decode(await rec.c.arrayBuffer());

    // 逐个顺序应用：后一个 edit 匹配前一个应用后的内容；
    // 失败条目记录 edit[i]: 原因，不阻塞其余 edit（部分成功语义）。
    let applied = 0;
    const failed = [];
    for (let i = 0; i < edits.length; i++) {
      const e = edits[i];
      const idx = `edit[${i + 1}]`;
      if (!e || !e.oldText) { failed.push(`${idx}: oldText is required`); continue; }
      if (e.newText === e.oldText) {
        failed.push(`${idx}: newText must be different from oldText`); continue;
      }
      const old = e.oldText;
      const first = content.indexOf(old);
      if (first < 0) { failed.push(`${idx}: oldText not found in file${doubleEncodingHint(content, old)}`); continue; }
      if (content.indexOf(old, first + old.length) >= 0) {
        const n = content.split(old).length - 1;
        failed.push(`${idx}: oldText matches ${n} locations; provide more surrounding context to make it unique`);
        continue;
      }
      content = content.slice(0, first) + e.newText + content.slice(first + old.length);
      applied++;
    }
    if (applied === 0) {
      if (failed.length === 1) throw fsErr("edit", failed[0]);
      throw fsErr("edit", `no edits applied: ${failed.join("; ")}`);
    }
    const db = await this._ensureDB();
    try {
      await idbPut(db, this._storeOf(abs), abs, { c: content, m: Date.now() });
    } catch (e) {
      throw fsErr("edit", e?.message || String(e));
    }
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

  // ---- 前端层底层方法（供 page_fs 操作对象与内置 exec 指令使用，
  //      不走 NATS 协议信封：文本原文 / Blob 直取 / key 枚举）----

  // resolve 公开路径展开：$VAR → 绝对路径 + 双根收容（与 run 同源）
  resolve(rawPath, ctx = {}) {
    const env = this._env(ctx.sessionId || "", ctx.workdir);
    return this._resolve(String(rawPath || ""), env, "resolve");
  }

  // readRaw 前端友好读：文本返回原文 string，二进制返回 Blob（附 mime/size）
  async readRaw(path, ctx = {}) {
    const abs = this.resolve(path, ctx);
    const rec = await this._getFile(abs, "read");
    if (rec.c instanceof Blob) {
      const head = new Uint8Array(await rec.c.slice(0, 512).arrayBuffer());
      const mime = detectMIME(head, abs);
      return { content: rec.c, mime, size: rec.c.size, path: abs };
    }
    return { content: rec.c, mime: "text/plain", size: byteLen(rec.c), path: abs };
  }

  // writeBlob 二进制写入（§2.2：browser 截图等本地产出物落 host fs，
  // agent 经 fs.read 读图；前端层底层方法，不走协议信封）。返回 {ok, path, size}。
  async writeBlob(path, blob, ctx = {}) {
    if (!(blob instanceof Blob)) throw fsErr("write", "writeBlob: blob is required");
    const abs = this.resolve(path, ctx);
    const db = await this._ensureDB();
    try {
      await idbPut(db, this._storeOf(abs), abs, { c: blob, m: Date.now() });
    } catch (e) {
      throw fsErr("write", e?.message || String(e));
    }
    return { ok: true, path: abs, size: blob.size };
  }

  // writeText 文本写入（vcmd curl 落盘用，§5.4）：内部走 write 语义，返回 {ok,path,size}。
  async writeText(path, text, ctx = {}) {
    const r = await this.run({ action: "write", path, content: String(text) }, ctx);
    return { ok: true, path: r.attrs?.path || path, size: Number(r.attrs?.bytes ?? 0) };
  }

  // list 目录列举：IndexedDB 无目录概念，由全部 key 前缀推导一层子项
  // （深层 key 折叠为目录项；目录无 size）。返回 {ok, path, items}。
  async list(path, ctx = {}) {
    const abs = this.resolve(path, ctx);
    // 虚拟根 /：双 store 物理隔离，单 store 扫描无法覆盖——返回两个顶级根
    if (abs === "/") {
      return {
        ok: true, path: "/",
        items: [
          { name: "home", path: "/home/", dir: true, size: undefined, mtime: undefined },
          { name: "sessions", path: "/sessions/", dir: true, size: undefined, mtime: undefined },
        ],
      };
    }
    const db = await this._ensureDB();
    const store = this._storeOf(abs);
    const prefix = abs.endsWith("/") ? abs : abs + "/";
    const keys = await idbAllKeys(db, store);
    const seen = new Map(); // seg -> {name, path, dir, size, mtime}
    for (const k of keys) {
      if (!k.startsWith(prefix)) continue;
      const rest = k.slice(prefix.length);
      if (!rest) continue;
      const seg = rest.split("/")[0];
      const dir = rest.includes("/");
      if (!seen.has(seg)) {
        seen.set(seg, {
          name: seg,
          path: prefix + seg + (dir ? "/" : ""),
          dir,
          size: undefined,
          mtime: undefined,
        });
      }
      if (!dir) {
        const rec = await idbGet(db, store, k);
        if (rec) {
          const it = seen.get(seg);
          it.size = rec.c instanceof Blob ? rec.c.size : byteLen(rec.c);
          it.mtime = rec.m;
        }
      }
    }
    // 排序对齐 vcore（§5.4）：UTF-8 字节序、目录不优先（目录名带 / 后缀自然参与排序）
    const items = [...seen.values()].sort((a, b) => cmpBytes(a.name, b.name));
    return { ok: true, path: prefix, items };
  }

  // stat 单路径状态（vcmd 虚拟指令用）：文件返回 {path,dir:false,size,mtime}；
  // 目录由子 key 前缀推导 {path,dir:true,size:0,mtime:undefined}（IndexedDB 无目录记录）；
  // $USER/$SESSION 根恒存在（无记录也是有效目录）；均不存在返回 null
  // （与 vcore Stat 语义对齐：不存在报错由调用方处理）。
  async stat(path, ctx = {}) {
    const abs = this.resolve(path, ctx);
    // 根恒存在：/（虚拟根）、/sessions、$USER、$SESSION——IndexedDB 无目录记录，
    // 但根概念有效（空工作区 ls 得 empty directory）
    if (abs === "/" || abs === "/sessions" ||
        abs === `/home/${this._uid}` || (ctx.sessionId && abs === `/sessions/${ctx.sessionId}`)) {
      return { path: abs, dir: true, size: 0, mtime: undefined };
    }
    const db = await this._ensureDB();
    const store = this._storeOf(abs);
    const rec = await idbGet(db, store, abs);
    if (rec !== undefined) {
      return {
        path: abs,
        dir: false,
        size: rec.c instanceof Blob ? rec.c.size : byteLen(rec.c),
        mtime: rec.m,
      };
    }
    const prefix = abs.endsWith("/") ? abs : abs + "/";
    const keys = await idbAllKeys(db, store);
    if (keys.some((k) => k.startsWith(prefix))) {
      return { path: prefix, dir: true, size: 0, mtime: undefined };
    }
    return null;
  }

  // walk 递归全量列举（vcmd 的 rg/tree 用）：返回平铺 {path,dir,size,mtime}[]，
  // 目录由全部 key 前缀推导（含深层中间目录）；不做隐藏/skip 过滤——过滤是
  // vcore 指令语义（vcmd.js），fs 层只提供原始树。
  async walk(path, ctx = {}) {
    const abs = this.resolve(path, ctx);
    const db = await this._ensureDB();
    const store = this._storeOf(abs);
    const prefix = abs.endsWith("/") ? abs : abs + "/";
    const keys = await idbAllKeys(db, store);
    const seen = new Map(); // full path -> {path, dir, size, mtime}
    for (const k of keys) {
      if (!k.startsWith(prefix)) continue;
      const rec = await idbGet(db, store, k);
      seen.set(k, {
        path: k,
        dir: false,
        size: rec?.c instanceof Blob ? rec.c.size : byteLen(rec?.c ?? ""),
        mtime: rec?.m,
      });
    }
    const dirs = new Set();
    for (const k of seen.keys()) {
      const rest = k.slice(prefix.length);
      if (!rest) continue;
      let idx = rest.indexOf("/");
      while (idx >= 0) {
        dirs.add(prefix + rest.slice(0, idx) + "/");
        idx = rest.indexOf("/", idx + 1);
      }
    }
    for (const d of dirs) seen.set(d, { path: d, dir: true, size: 0, mtime: undefined });
    return { ok: true, path: prefix, items: [...seen.values()] };
  }

  // remove 删除（对齐 vcore rm 语义）：文件/无子项直接删；有子项（非空目录）
  // 需 ctx.recursive（-r）否则报错；递归时删除全部子 key。
  // IndexedDB 无目录记录：空目录与不存在不可区分 → 均报 no such file（模型固有限制）。
  async remove(path, ctx = {}) {
    const abs = this.resolve(path, ctx);
    const db = await this._ensureDB();
    const store = this._storeOf(abs);
    const rec = await idbGet(db, store, abs);
    if (rec !== undefined) {
      await idbDelete(db, store, abs);
      return { ok: true, removed: abs };
    }
    const prefix = abs.endsWith("/") ? abs : abs + "/";
    const keys = await idbAllKeys(db, store);
    const children = keys.filter((k) => k.startsWith(prefix));
    if (children.length > 0) {
      if (!ctx.recursive) throw fsErr("rm", `${abs} is a non-empty directory (use -r)`);
      for (const k of children) await idbDelete(db, store, k);
      return { ok: true, removed: abs, items: children.length };
    }
    throw fsErr("rm", `${abs}: no such file or directory`);
  }

  // exists 存在性检查。返回 boolean。
  async exists(path, ctx = {}) {
    const abs = this.resolve(path, ctx);
    const db = await this._ensureDB();
    const rec = await idbGet(db, this._storeOf(abs), abs);
    return rec !== undefined;
  }
}
