/**
 * argv.js — Unix 风格 argv 双层解析（§2.1），与 aic libs/tools.ParseActionArgv
 * 及 sdk/argv.go (Go) 逐行为一致：
 *
 * 第一层（通用切分）：
 *   - "--" 终止符：其后所有元素一律进入 positional
 *   - 以 "--" 开头且去前缀后匹配 ^[a-zA-Z][a-zA-Z0-9_-]*$ 的元素为候选 flag
 *   - "--name=value" 且 name 部分为合法 flag 名 → invalid flag 错误
 *   - 其余元素（含单横线 -i、---、--TODO 等）进入 positional
 *
 * 第二层（action 级 flag 表校验）：
 *   - 未知 flag → unknown flag 错误（附 supported 列表）
 *   - value flag 消费下一非候选 flag、非 "--" 元素为值，否则报 requires a value
 *   - bool flag 不消费下一元素
 */

const FLAG_NAME_RE = /^[a-zA-Z][a-zA-Z0-9_-]*$/;

export function looksLikeFlagName(key) {
  return FLAG_NAME_RE.test(key);
}

function looksLikeCandidateFlag(arg) {
  if (!arg.startsWith("--")) return false;
  const key = arg.slice(2);
  const eq = key.indexOf("=");
  if (eq >= 0) return looksLikeFlagName(key.slice(0, eq));
  return looksLikeFlagName(key);
}

function supportedFlagsSuffix(flags) {
  const names = Object.keys(flags).map((n) => "--" + n).sort();
  if (names.length === 0) return "";
  return " (supported: " + names.join(", ") + ")";
}

/**
 * parseActionArgv(tool, action, argv, flags)
 * flags: { name: "bool" | "value" }
 * 返回 { positional: [], flags: {}, bools: {} }；解析失败 throw Error（"{tool} {action}: {原因}"）。
 */
export function parseActionArgv(tool, action, argv, flags) {
  const pa = { positional: [], flags: {}, bools: {} };
  let terminated = false;
  for (let i = 0; i < argv.length; i++) {
    const arg = argv[i];
    if (terminated) {
      pa.positional.push(arg);
      continue;
    }
    if (arg === "--") {
      terminated = true;
      continue;
    }
    if (!arg.startsWith("--")) {
      pa.positional.push(arg);
      continue;
    }
    const key = arg.slice(2);
    const eq = key.indexOf("=");
    if (eq >= 0 && looksLikeFlagName(key.slice(0, eq))) {
      const name = key.slice(0, eq);
      throw new Error(`${tool} ${action}: invalid flag "${arg}"; use "--${name} <value>" (two separate argv elements)`);
    }
    if (!looksLikeFlagName(key)) {
      pa.positional.push(arg);
      continue;
    }
    const kind = flags[key];
    if (kind === undefined) {
      throw new Error(`${tool} ${action}: unknown flag "--${key}"${supportedFlagsSuffix(flags)}`);
    }
    if (kind === "bool") {
      pa.bools[key] = true;
      continue;
    }
    if (i + 1 < argv.length && argv[i + 1] !== "--" && !looksLikeCandidateFlag(argv[i + 1])) {
      pa.flags[key] = argv[i + 1];
      i++;
      continue;
    }
    throw new Error(`${tool} ${action}: flag "--${key}" requires a value`);
  }
  return pa;
}
