#!/usr/bin/env node
/**
 * sync-browser.mjs — 把插件端 browser 共享代码同步到 desktop/vendor/browser/
 *
 * 共享布局（core.js 内的相对 import `../../sdk/argv.js` 依赖目录结构不变）：
 *   vendor/browser/tools/browser/core.js      ← browser/src/tools/browser/core.js
 *   vendor/browser/sdk/argv.js                ← browser/src/sdk/argv.js
 *   vendor/browser/content/network-interceptor.js ← browser/src/content/...
 *
 * npm prestart/predist 钩子自动运行（dev 启动与打包前均同步），幂等。
 */
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

const desktopDir = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const srcRoot = path.join(desktopDir, "..", "browser", "src");
const dstRoot = path.join(desktopDir, "vendor", "browser");

const FILES = [
  "tools/browser/core.js",
  "sdk/argv.js",
  "content/network-interceptor.js",
];

let synced = 0;
for (const rel of FILES) {
  const src = path.join(srcRoot, rel);
  const dst = path.join(dstRoot, rel);
  const data = fs.readFileSync(src);
  if (fs.existsSync(dst) && fs.readFileSync(dst).equals(data)) continue;
  fs.mkdirSync(path.dirname(dst), { recursive: true });
  fs.writeFileSync(dst, data);
  synced++;
  console.log(`[sync-browser] ${rel}`);
}
if (synced === 0) console.log("[sync-browser] up to date");
