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
 * parseRequest 解析工具请求信封（§6.2）。data 字段保持原始 JSON 字符串
 * （验签的 data_sha256 是对原始字节的哈希，禁止重序列化）。
 * 返回 null 表示非法 JSON。
 */
export function parseRequest(data) {
  try {
    const req = JSON.parse(data);
    if (!req || typeof req !== "object") return null;
    return req;
  } catch (_) {
    return null;
  }
}

/**
 * buildCaps 构造 caps v2（§6.3）：
 *   - fsActions：null = 全部 3 个 action；[] = 不支持 fs
 *   - programs：null = 不限制（物理 host 按 PATH 自检）；[] = 纯虚拟（browser 类 host 必须显式）
 *   - virtual：exec 虚拟指令声明 [{name, required_level, stateful?, backgroundable?}]
 */
export function buildCaps({ hostID, credVer, version, deviceType, deviceName, fsActions = [], programs = [], virtual = [] }) {
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
    exec: { programs, virtual },
  };
  // null 形态显式序列化为 null（JSON.stringify 保留 undefined 会丢键，用 null）
  if (fsActions === null) caps.fs.actions = null;
  if (programs === null) caps.exec.programs = null;
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
