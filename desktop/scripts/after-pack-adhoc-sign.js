/**
 * after-pack-adhoc-sign.js — electron-builder afterPack 钩子：macOS ad-hoc 深签
 *
 * 背景（2026-09-14）：测试版无 Developer ID 证书，mac 段 identity: null 跳过
 * electron-builder 自带签名 → 下载包带 quarantine 时 macOS 报「已损坏」（需
 * xattr 清隔离，无法右键放行）。本钩子对 .app 做 `codesign --force --deep
 * --sign -` 深签（社区对 electron-builder 26 直接 ad-hoc 会破坏相机/麦克风的
 * workaround，见 electron-builder#9529）：下载打开提示变为「未验证开发者」
 * （系统设置 → 隐私与安全性 → 仍要打开 可放行），且不引入 hardened runtime，
 * 保留麦克风/相机权限路径。
 *
 * 所有平台先校验 Chrome 资源，darwin 额外签名；校验/签名失败即构建失败。
 *
 * 注：彻底免用户放行需 Developer ID 签名 + 公证（需苹果开发者账号）。
 */
"use strict";

const { execFileSync } = require("node:child_process");
const path = require("node:path");
const fs = require("node:fs");
const { Arch } = require("builder-util");

module.exports = async (context) => {
  const { assertBrowserBundle } = await import("./sync-browser.mjs");
  const resources = context.electronPlatformName === "darwin"
    ? path.join(context.appOutDir, `${context.packager.appInfo.productFilename}.app`, "Contents", "Resources")
    : path.join(context.appOutDir, "resources");
  assertBrowserBundle(path.join(resources, "browser"), context.electronPlatformName, Arch[context.arch]);
  const { assertAgentBrowserBundle } = await import("./sync-agent-browser.mjs");
  assertAgentBrowserBundle(path.join(resources, "agent-browser"), context.electronPlatformName, Arch[context.arch]);
  const cua = JSON.parse(fs.readFileSync(path.join(context.packager.projectDir, "cua.json")));
  for (const name of [...cua.assets[context.electronPlatformName].keep, "LICENSE", "THIRD_PARTY_NOTICES.md"]) {
    if (!fs.existsSync(path.join(resources, "cua", context.electronPlatformName, name)))
      throw new Error(`CuaDriver resource missing: ${name}`);
  }
  if (context.electronPlatformName !== "darwin") return;
  const appPath = path.join(context.appOutDir, `${context.packager.appInfo.productFilename}.app`);
  if (!fs.existsSync(appPath)) {
    throw new Error(`[adhoc-sign] app bundle not found: ${appPath}`);
  }
  execFileSync("codesign", ["--force", "--deep", "--sign", "-", appPath], { stdio: "inherit" });
  execFileSync("codesign", ["--verify", "--deep", "--strict", appPath], { stdio: "inherit" });
  console.log(`[adhoc-sign] ad-hoc deep-signed ok: ${appPath}`);
};
