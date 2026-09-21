#!/usr/bin/env node
/**
 * check-asar.mjs — 打包产物完整性校验（make desktop-* 与 CI 共用）
 *
 * 背景（2026-09-14，v0.6.3 两起真实事故）：
 *   ① electron-builder.yml 的 files 白名单漏了新文件（leader-grab.js）——
 *      main.js 启动即 require('./leader-grab')，缺则主进程直接崩溃；
 *   ② 壳依赖缺失时，启动失败。
 * 本脚本在打包后校验：各入口（main.js / browser-path.cjs）
 * 的递归相对 require/import 全部能在 asar 中解析，且 resources/backend 后端二进制存在。
 * 不通过 → exit 1（CI / 本地 make 直接失败，防同类遗漏再发版）。
 *
 * 用法：node scripts/check-asar.mjs [app.asar 路径]
 *   缺省：在 ../dist 下自动查找 app.asar（多个取最新 mtime）。
 */
import fs from "node:fs";
import path from "node:path";
import { createRequire } from "node:module";
import { fileURLToPath } from "node:url";

const require = createRequire(import.meta.url);
const asar = require("@electron/asar");
const { assertBrowserBundle, manifest } = require("../browser-bundle.cjs");

const desktopDir = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const distDir = path.resolve(desktopDir, "..", "dist");

// ---- 定位 app.asar ----
function findAsar(dir) {
  const found = [];
  const walk = (d) => {
    let entries;
    try {
      entries = fs.readdirSync(d, { withFileTypes: true });
    } catch {
      return;
    }
    for (const e of entries) {
      const p = path.join(d, e.name);
      if (e.isDirectory()) walk(p);
      else if (e.name === "app.asar") found.push(p);
    }
  };
  walk(dir);
  if (!found.length) return null;
  found.sort((a, b) => fs.statSync(b).mtimeMs - fs.statSync(a).mtimeMs);
  return found[0];
}

const asarPath = process.argv[2] ? path.resolve(process.argv[2]) : findAsar(distDir);
if (!asarPath || !fs.existsSync(asarPath)) {
  console.error(
    `[check-asar] ✗ app.asar not found${process.argv[2] ? `: ${process.argv[2]}` : ` under ${distDir}`}`,
  );
  process.exit(1);
}

// ---- asar 条目集合 ----
// @electron/asar 的 listFiles 用 path.join 拼条目路径：Windows 为反斜杠分隔
// （\main.js），posix 为 / 分隔——统一归一为 / 前缀的 posix 形式再比对。
const normEntry = (p) => {
  const s = String(p).replace(/\\/g, "/");
  return s.startsWith("/") ? s : "/" + s;
};
const entries = new Set(asar.listPackage(asarPath).map(normEntry));
if (!entries.size) {
  console.error(`[check-asar] ✗ ${asarPath} 条目列表为空（asar 解析异常？）`);
  process.exit(1);
}

// ---- 入口文件与其相对 require/import 逐条解析 ----
const ENTRIES = ["main.js", "browser-path.cjs"];
const RE = /(?:require\(\s*|from\s+|import\(\s*|import\s+)["']([^"']+)["']/g;
const missing = [];

const queue = ENTRIES.map(name => "/" + name), visited = new Set();
while (queue.length) {
  const self = queue.shift();
  if (visited.has(self)) continue;
  visited.add(self);
  if (!entries.has(self)) { missing.push(`${self}（模块未进包）`); continue; }
  const src = asar.extractFile(asarPath, self.slice(1)).toString("utf8");
  for (const m of src.matchAll(RE)) {
    const spec = m[1];
    if (!spec.startsWith("./") && !spec.startsWith("../")) continue;
    const base = path.posix.normalize(path.posix.join(path.posix.dirname(self), spec));
    const candidate = [base, `${base}.js`, `${base}.mjs`, `${base}.cjs`, `${base}/index.js`].find(c => entries.has(c));
    if (!candidate) missing.push(`${self} → ${spec}`);
    else if (/\.(mjs|cjs|js)$/.test(candidate)) queue.push(candidate);
  }
}

if ([...entries].some(x=>x.startsWith("/vendor/browser/"))) missing.push("Chrome belongs in resources/browser, outside asar");

// ---- 设置页（app://aic → desktop/settings-ui，单文件静态页）随包 ----
if (!entries.has("/settings-ui/settings.html")) missing.push("设置页缺失: /settings-ui/settings.html");

// ---- resources/backend 后端二进制 ----
const resDir = path.dirname(asarPath); // mac: Contents/Resources；win/linux: resources
const backendCandidates = [
  path.join(resDir, "backend", "aic-backend"),
  path.join(resDir, "backend", "aic-backend.exe"),
];
if (!backendCandidates.some((p) => fs.existsSync(p))) {
  missing.push("resources/backend/aic-backend(.exe)");
}

// Chrome is a required independent runtime, outside asar. Validate the target
// and its resources, rather than allowing a system Chrome fallback to hide a
// broken release. Old JS engines and other architectures must not ship.
try {
  const root = path.join(resDir, "browser"), targets = fs.readdirSync(root);
  if (targets.length !== 1 || !manifest.assets[targets[0]]) throw new Error("expected exactly one supported browser target");
  const [platform, arch] = targets[0].split("-");
  assertBrowserBundle(root, platform, arch);
} catch (error) { missing.push(`resources/browser: ${error.message}`); }

// ---- 结果 ----
if (missing.length) {
  console.error(`[check-asar] ✗ ${path.relative(desktopDir, asarPath)} 校验失败：`);
  for (const m of missing) console.error(`  - 缺失/无法解析: ${m}`);
  process.exit(1);
}
console.log(
  `[check-asar] ✓ ${path.relative(desktopDir, asarPath)}（入口相对引用与后端资源齐备）`,
);
