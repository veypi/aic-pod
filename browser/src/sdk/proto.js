/**
 * proto.js — 指令集 v2 协议层（与 aic-pod/sdk/proto Go 包对齐，零漂移）
 *
 * 会话级 subject 构造/解析、请求信封解析、caps v2 构建。纯函数，不依赖 NATS，
 * 可独立单测（固定向量见 sdk/proto.test.js，与 Go vectors_test.go 同源）。
 *
 * 禁止双端各自另写 subject/信封格式——改动必须同步 Go 侧并更新固定向量。
 */

// 执行目标保留字（§1.1）：无需解析为物理 host。
export const HOST_CLOUD = "cloud";
export const HOST_PAGE = "page";

// 指令集名（§6.2）：信封 tool 字段与 subject 段一致。
export const TOOL_FS = "fs";
export const TOOL_EXEC = "exec";

// 会话级 subject 中物理 host 段的强制前缀（§6.1）：
// 前端 JWT 的 sub deny（u.{uid}.s.*.h.host_*.>）依赖此前缀精确匹配物理 host
// 工具流量而不误伤 h.page.>。host_id 本身不带前缀（1host 参数/连接级 subject 用裸 id）。
export const HOST_ID_PREFIX = "host_";

function validSeg(s) {
  return typeof s === "string" && s !== "" && !/[.*> \t\r\n]/.test(s);
}

/** hostSeg 构造会话级 subject 的 host 段：物理 host 加前缀，page/cloud/已带前缀原样（幂等）。 */
export function hostSeg(host) {
  if (host === HOST_CLOUD || host === HOST_PAGE || host.startsWith(HOST_ID_PREFIX)) {
    return host;
  }
  return HOST_ID_PREFIX + host;
}

/** ToolReqSubject：u.{uid}.s.{sid}.h.{host}.{tool}.req（host 段自动加前缀）。 */
export function toolReqSubject(uid, sid, host, tool) {
  if (!validSeg(uid) || !validSeg(sid) || !validSeg(host)) {
    throw new Error(`proto: invalid uid/sid/host segment (uid=${uid} sid=${sid} host=${host})`);
  }
  if (tool !== TOOL_FS && tool !== TOOL_EXEC) {
    throw new Error(`proto: invalid tool ${tool} (must be fs|exec)`);
  }
  return `u.${uid}.s.${sid}.h.${hostSeg(host)}.${tool}.req`;
}

/** HostInboxSubject：host 端连接时的单订阅 u.{uid}.s.*.h.host_{host_id}.>。 */
export function hostInboxSubject(uid, hostID) {
  if (!validSeg(uid) || !validSeg(hostID)) {
    throw new Error(`proto: invalid uid/hostID segment`);
  }
  return `u.${uid}.s.*.h.${hostSeg(hostID)}.>`;
}

/**
 * ParseToolReqSubject 解析会话级工具请求 subject（8 段）：
 * u.{uid}.s.{sid}.h.{host}.{tool}.req → {uid, sid, host(还原裸 id), tool}。
 * 非法 subject 返回 null。
 */
export function parseToolReqSubject(subject) {
  const parts = String(subject || "").split(".");
  if (parts.length !== 8 || parts[0] !== "u" || parts[2] !== "s" || parts[4] !== "h" || parts[7] !== "req") {
    return null;
  }
  const tool = parts[6];
  if (tool !== TOOL_FS && tool !== TOOL_EXEC) {
    return null;
  }
  const host = parts[5];
  return {
    uid: parts[1],
    sid: parts[3],
    host: host.startsWith(HOST_ID_PREFIX) ? host.slice(HOST_ID_PREFIX.length) : host,
    tool,
  };
}

/**
 * parseRequest 解析工具请求信封（§6.2）。data 字段从信封原始文本中按字节
 * 提取为原始 JSON 字符串：验签的 data_sha256 覆盖服务端序列化的原始字节，
 * JSON.parse 后重序列化会破坏字节形态（空白/键序/转义差异），严格禁止。
 * 返回 null 表示非法 JSON 或缺少 data 成员。
 */
export function parseRequest(data) {
  let req;
  try {
    req = JSON.parse(data);
  } catch (_) {
    return null;
  }
  if (!req || typeof req !== "object") return null;
  const rawData = extractRawMember(data, "data");
  if (rawData === null) return null;
  req.data = rawData;
  return req;
}

// ---- 信封 data 成员的字节级提取（不经过 JSON.parse 物化） ----

/**
 * extractRawMember 从顶层 JSON 对象文本中提取指定成员的原始值子串。
 * 只扫描顶层成员；key 按 JSON 解码后比较（兼容转义写法）。
 * 返回 null 表示未找到或文本结构非法。
 */
function extractRawMember(text, key) {
  const n = text.length;
  let i = 0;
  const skipWs = () => {
    while (i < n && (text[i] === " " || text[i] === "\t" || text[i] === "\r" || text[i] === "\n")) i++;
  };
  skipWs();
  if (text[i] !== "{") return null;
  i++;
  for (;;) {
    skipWs();
    if (i >= n || text[i] === "}") return null;
    if (text[i] !== '"') return null;
    const keyStart = i;
    i = scanString(text, i);
    if (i < 0) return null;
    let k;
    try {
      k = JSON.parse(text.slice(keyStart, i));
    } catch (_) {
      return null;
    }
    skipWs();
    if (text[i] !== ":") return null;
    i++;
    skipWs();
    const valStart = i;
    i = scanValue(text, i);
    if (i < 0) return null;
    if (k === key) return text.slice(valStart, i);
    skipWs();
    if (text[i] !== ",") return null; // "}" 或非法：未找到目标成员
    i++;
  }
}

/** scanString 从 text[start]==='"' 起扫描 JSON 字符串，返回闭引号后下标；非法返回 -1。 */
function scanString(text, start) {
  let i = start + 1;
  while (i < text.length) {
    const c = text[i];
    if (c === "\\") {
      i += 2;
      continue;
    }
    if (c === '"') return i + 1;
    i++;
  }
  return -1;
}

/** scanValue 从 start 扫描完整 JSON 值（对象/数组/字符串/字面量），返回结束后下标；非法返回 -1。 */
function scanValue(text, start) {
  const c = text[start];
  if (c === '"') return scanString(text, start);
  if (c === "{" || c === "[") {
    let depth = 0;
    let i = start;
    while (i < text.length) {
      const ch = text[i];
      if (ch === '"') {
        i = scanString(text, i);
        if (i < 0) return -1;
        continue;
      }
      if (ch === "{" || ch === "[") {
        depth++;
      } else if (ch === "}" || ch === "]") {
        depth--;
        if (depth === 0) return i + 1;
      }
      i++;
    }
    return -1;
  }
  // 数字 / true / false / null：扫描到分隔符
  let i = start;
  while (i < text.length && !",}] \t\r\n".includes(text[i])) i++;
  return i > start ? i : -1;
}

/**
 * buildCaps 构造 caps v2（§6.3）：
 *   - fsActions：null = 全部 3 个 action；[] = 不支持 fs
 *   - commands：exec 统一命令声明表 [{name, desc?, help?, level}]（§6.3，
 *     与 Go sdk/proto.CommandDecl 同构）；未声明的命令服务端一律拒绝
 */
export function buildCaps({ hostID, credVer, version, deviceType, deviceName, fsActions = [], commands = [] }) {
  const caps = {
    host_id: hostID,
    credential_ver: credVer,
    agent_version: version,
    device_type: deviceType,
    hostname: deviceName,
    device_info: {
      os: "Chrome",
      arch: "browser",
      num_cpu: (typeof navigator !== "undefined" && navigator.hardwareConcurrency) || 1,
    },
    fs: { actions: fsActions },
    exec: { commands },
  };
  // null 形态显式序列化为 null（JSON.stringify 保留 undefined 会丢键，用 null）
  if (fsActions === null) caps.fs.actions = null;
  return caps;
}

/** 权限等级（§2.4，与 Go proto.Level* 对齐）。 */
export const Level = {
  NONE: 0,
  READ: 1,
  WRITE: 2,
  DANGER: 3,
  CRITICAL: 4,
  APPROVED: 9,
};
