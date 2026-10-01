// shellQuote 是前端构造 vsh 脚本的唯一转义点（hosts-vsh-redesign §5）：
// viewer/页面代码拼接脚本参数必须经此函数，禁止各自实现引号处理。
// POSIX 单引号语义：'...' 内无转义序列；单引号本身用 '"'"' 闭合替换。
// 良名（无空白/无 shell 元字符）直通以保持脚本可读。
//
// 防护边界（调用契约）：
//   - `-` 开头的值（如 --accept、-rf）不会被防护——它匹配良名直通，经
//     cliargs 解析会被当成旗标。调用点在用户可控位置参数前必须插字面量
//     `--`（aic-pod cliargs：`--` 后全按位置参数），旗标值用 `--key=value`
//     形式（`=` 后任意值 verbatim，不会被当旗标）。
//   - NUL 不可表达（execve 语义），直接抛错。
export function shellQuote(value) {
  const s = String(value);
  if (s.includes("\0")) throw new Error("shellQuote: NUL not allowed");
  if (s === "") return "''";
  if (/^[A-Za-z0-9_\-./:=@%+,]+$/.test(s)) return s;
  return "'" + s.replace(/'/g, "'\"'\"'") + "'";
}

// shellFlags 把 {key: value} 对象转成 CLI flag 序列（--key value）：
// true → 仅旗标；false/null/undefined → 跳过；数组 → 逐项重复旗标；
// 其余一律经 shellQuote。key 必须是小写良名（防注入面——key 不转义）。
export function shellFlags(args) {
  const parts = [];
  for (const [key, value] of Object.entries(args || {})) {
    if (value === false || value === null || value === undefined) continue;
    if (!/^[a-z][a-z0-9_]*$/.test(key))
      throw new Error(`shellFlags: invalid flag name ${key}`);
    if (value === true) {
      parts.push("--" + key);
    } else if (Array.isArray(value)) {
      for (const item of value) parts.push("--" + key, shellQuote(item));
    } else {
      parts.push("--" + key, shellQuote(value));
    }
  }
  return parts.join(" ");
}
