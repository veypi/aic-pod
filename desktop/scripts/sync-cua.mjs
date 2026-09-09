#!/usr/bin/env node
/**
 * sync-cua.mjs — 把固定版本的 cua-driver 发行物同步到 desktop/vendor/cua/<platform>/
 *
 * 用途：内置进安装包（electron-builder extraResources → resources/cua/），运行时由
 * main.js 注入 CUA_DRIVER_PATH / CUA_DRIVER_APP 给 Go 后端（libs/host/cua.go）。
 *
 * 版本固定 + sha256 校验：desktop/cua.json（升级 = 改 tag/sha256 后重跑；--force 强制重下）。
 * 布局（vendor/ 已 gitignore，不进仓库）：
 *   vendor/cua/darwin/CuaDriver.app + LICENSE.md  ← darwin-universal.tar.gz
 *     （Developer ID 签名 + 公证，ditto 原样拷贝保持签名；TCC 授权归 com.trycua.driver）
 *   vendor/cua/win32/cua-driver.exe + LICENSE.md  ← windows-x86_64.zip
 *   vendor/cua/linux/cua-driver + LICENSE.md      ← linux-x86_64.tar.gz
 *   （发行包根目录还带 SDK dylib/dll/头文件/node runtime，只取 cua.json 的 keep
 *     集合；许可证文本上游包内不含，从仓库固定副本 desktop/cua-LICENSE.md 复制）
 *
 * 幂等：vendor/cua/.stamp-<platform> 记录 tag+file+sha256，命中即跳过
 * （stamp 放平台目录外，不随安装包分发）。
 */
import crypto from "node:crypto";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { execFileSync } from "node:child_process";
import { fileURLToPath } from "node:url";

const desktopDir = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const cfg = JSON.parse(fs.readFileSync(path.join(desktopDir, "cua.json"), "utf8"));

const platformKey =
  process.platform === "darwin" ? "darwin" : process.platform === "win32" ? "win32" : "linux";
const asset = cfg.assets[platformKey];
if (!asset) {
  console.error(`[sync-cua] desktop/cua.json 缺少 ${platformKey} 资产配置`);
  process.exit(1);
}

const destDir = path.join(desktopDir, "vendor", "cua", platformKey);
const stampPath = path.join(desktopDir, "vendor", "cua", `.stamp-${platformKey}`);
const stamp = `${cfg.tag} ${asset.file} ${asset.sha256}`;
const force = process.argv.includes("--force");

if (!force && fs.existsSync(stampPath) && fs.readFileSync(stampPath, "utf8").trim() === stamp) {
  console.log(`[sync-cua] up to date: cua-driver ${cfg.version} (${platformKey})`);
  process.exit(0);
}

const url = `https://github.com/${cfg.repo}/releases/download/${cfg.tag}/${asset.file}`;
const assetArgIdx = process.argv.indexOf("--asset");
const localAsset = assetArgIdx > -1 ? process.argv[assetArgIdx + 1] : process.env.CUA_DRIVER_ASSET || "";

const tmp = fs.mkdtempSync(path.join(os.tmpdir(), "aic-cua-"));
try {
  const archive = path.join(tmp, asset.file);
  if (localAsset) {
    console.log(`[sync-cua] 本地资产: ${localAsset}`);
    fs.copyFileSync(localAsset, archive);
  } else {
    console.log(`[sync-cua] download cua-driver ${cfg.version}: ${asset.file}`);
    await download(archive, url);
  }
  const sha = crypto.createHash("sha256").update(fs.readFileSync(archive)).digest("hex");
  if (sha !== asset.sha256) {
    console.error(`[sync-cua] sha256 校验失败\n  got  ${sha}\n  want ${asset.sha256}`);
    process.exit(1);
  }

  const extractDir = path.join(tmp, "x");
  fs.mkdirSync(extractDir);
  if (asset.file.endsWith(".zip")) {
    if (process.platform === "win32") {
      execFileSync(
        "powershell",
        ["-NoProfile", "-Command", `Expand-Archive -LiteralPath '${archive}' -DestinationPath '${extractDir}' -Force`],
        { stdio: "inherit" }
      );
    } else {
      execFileSync("unzip", ["-q", archive, "-d", extractDir], { stdio: "inherit" });
    }
  } else {
    execFileSync("tar", ["-xzf", archive, "-C", extractDir], { stdio: "inherit" });
  }

  const payloadRoot = findPayloadRoot(extractDir, asset.payload);
  if (!payloadRoot) {
    console.error(`[sync-cua] ${asset.file} 内找不到 ${asset.payload}`);
    process.exit(1);
  }

  fs.rmSync(destDir, { recursive: true, force: true });
  fs.mkdirSync(destDir, { recursive: true });
  // 只取运行必需的 keep 集合（cua.json）：发行包根目录还带 SDK dylib/dll/头文件/
  // node runtime，不随包分发（darwin 解包 179MB → 仅 CuaDriver.app 后 65MB）。
  // win32 需 cua-driver-uia.exe（daemon 内部 UIAccess worker）与 cua-cursor-theme.exe；
  // linux 需 cua-cursor-theme 与 wayland-helper/（GNOME 扩展）。
  const keep = new Set(asset.keep || [asset.payload]);
  const missing = [...keep].filter((name) => !fs.existsSync(path.join(payloadRoot, name)));
  if (missing.length > 0) {
    console.error(`[sync-cua] ${asset.file} 内缺少：${missing.join(", ")}（上游布局变更？核对 cua.json keep）`);
    process.exit(1);
  }
  for (const name of fs.readdirSync(payloadRoot)) {
    if (keep.has(name) || /^LICENSE(\.|$)/i.test(name)) {
      copyEntry(path.join(payloadRoot, name), path.join(destDir, name));
    }
  }
  // MIT 许可证文本：上游发行包内不含（2026-09 实测 0.25.0），从仓库固定副本
  // desktop/cua-LICENSE.md 复制（MIT 要求随分发携带版权与许可声明）。
  fs.copyFileSync(path.join(desktopDir, cfg.licenseFile), path.join(destDir, "LICENSE.md"));
  fs.writeFileSync(stampPath, `${stamp}\n`);
  console.log(`[sync-cua] → ${path.relative(desktopDir, destDir)} (${asset.payload})`);
} finally {
  fs.rmSync(tmp, { recursive: true, force: true });
}

// download 先试 curl（走系统 CA/代理），失败回退 Node fetch；两者都失败给出
// --asset 手动通道提示（受限环境 / 离线 CI 用 CUA_DRIVER_ASSET 或 --asset <file>）。
async function download(dest, url) {
  try {
    execFileSync("curl", ["-fsSL", "--retry", "3", "--retry-delay", "2", "-o", dest, url], {
      stdio: ["ignore", "inherit", "inherit"],
    });
    return;
  } catch (err) {
    console.warn(`[sync-cua] curl 下载失败（exit ${err.status ?? err.message}），回退 Node fetch…`);
  }
  try {
    const res = await fetch(url, { redirect: "follow" });
    if (!res.ok) throw new Error(`HTTP ${res.status}`);
    fs.writeFileSync(dest, Buffer.from(await res.arrayBuffer()));
  } catch (err) {
    console.error(`[sync-cua] 下载失败：${err.message}\n  手动下载后重跑：node scripts/sync-cua.mjs --asset <下载的文件>\n  URL: ${url}`);
    process.exit(1);
  }
}

// findPayloadRoot 递归查找含 payload 的目录（发行包顶层目录名随版本变化，最多下钻 3 层）。
function findPayloadRoot(root, payloadName, depth = 0) {
  if (fs.existsSync(path.join(root, payloadName))) return root;
  if (depth >= 3) return null;
  for (const ent of fs.readdirSync(root, { withFileTypes: true })) {
    if (!ent.isDirectory()) continue;
    const hit = findPayloadRoot(path.join(root, ent.name), payloadName, depth + 1);
    if (hit) return hit;
  }
  return null;
}

// copyEntry 保留可执行位/符号链接（macOS 用 ditto：签名/xattr 原样，codesign 不失效）。
function copyEntry(from, to) {
  if (process.platform === "darwin") {
    execFileSync("ditto", [from, to], { stdio: "inherit" });
    return;
  }
  if (process.platform === "win32") {
    execFileSync(
      "powershell",
      ["-NoProfile", "-Command", `Copy-Item -LiteralPath '${from}' -Destination '${to}' -Recurse -Force`],
      { stdio: "inherit" }
    );
    return;
  }
  execFileSync("cp", ["-a", from, to], { stdio: "inherit" });
}
