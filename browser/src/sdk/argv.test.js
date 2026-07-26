// argv.js 双层解析测试（与 Go sdk/argv.go、aic libs/tools 行为一致）
import { test } from "node:test";
import assert from "node:assert/strict";
import { parseActionArgv, looksLikeFlagName } from "./argv.js";

const FLAGS = { content: "value", limit: "value", offset: "value", "replace-all": "bool", "ignore-case": "bool" };

test("basics: positional + value flag + bool flag", () => {
  const pa = parseActionArgv("fs", "edit", ["/a.txt", "--content", "x y", "--replace-all"], FLAGS);
  assert.equal(pa.flags["content"], "x y");
  assert.equal(pa.bools["replace-all"], true);
  assert.deepEqual(pa.positional, ["/a.txt"]);
});

test("bool flag 不吞下一元素", () => {
  const pa = parseActionArgv("fs", "search", ["--ignore-case", "/app"], FLAGS);
  assert.equal(pa.bools["ignore-case"], true);
  assert.deepEqual(pa.positional, ["/app"]);
});

test("-- 终止符后全部落位置参数", () => {
  const pa = parseActionArgv("fs", "read", ["--", "--limit", "5"], FLAGS);
  assert.deepEqual(pa.positional, ["--limit", "5"]);
  assert.deepEqual(pa.flags, {});
});

test("未知 flag 报错（附 supported）", () => {
  assert.throws(
    () => parseActionArgv("fs", "read", ["/a", "--lmit", "5"], FLAGS),
    (e) => e.message === 'fs read: unknown flag "--lmit" (supported: --content, --ignore-case, --limit, --offset, --replace-all)'
  );
});

test("--flag=value 统一报错", () => {
  assert.throws(
    () => parseActionArgv("fs", "read", ["--limit=5"], FLAGS),
    (e) => e.message === 'fs read: invalid flag "--limit=5"; use "--limit <value>" (two separate argv elements)'
  );
});

test("value flag 缺值/下一个是候选 flag 时报错", () => {
  assert.throws(() => parseActionArgv("fs", "read", ["/a", "--limit"], FLAGS),
    (e) => e.message === 'fs read: flag "--limit" requires a value');
  assert.throws(() => parseActionArgv("fs", "read", ["--limit", "--offset", "1"], FLAGS),
    (e) => e.message === 'fs read: flag "--limit" requires a value');
});

test("value flag 的值可以是任意文本（--- 开头不做 flag 检查）", () => {
  const content = "---\ndescription: demo\n---\nbody";
  const pa = parseActionArgv("fs", "write", ["/a.md", "--content", content], FLAGS);
  assert.equal(pa.flags["content"], content);
});

test("单横线/非字母开头/含非法字符的 -- 元素落入位置参数", () => {
  const pa = parseActionArgv("fs", "ls", ["-i", "---", "--1abc", "--foo bar", "/app"], FLAGS);
  assert.deepEqual(pa.positional, ["-i", "---", "--1abc", "--foo bar", "/app"]);
});

test("flag 名须字母开头", () => {
  const cases = { content: true, "replace-all": true, foo_bar: true, a1: true, "1abc": false, "-abc": false, _abc: false, "": false };
  for (const [k, want] of Object.entries(cases)) {
    assert.equal(looksLikeFlagName(k), want, k);
  }
});
