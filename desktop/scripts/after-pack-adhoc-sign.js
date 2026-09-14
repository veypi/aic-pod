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
 * 仅 darwin 生效（其他平台 no-op）；签名/校验失败即抛错（构建失败）。
 *
 * 注：彻底免用户放行需 Developer ID 签名 + 公证（需苹果开发者账号）。
 */
"use strict";

const { execFileSync } = require("node:child_process");
const path = require("node:path");
const fs = require("node:fs");

module.exports = async (context) => {
  if (context.electronPlatformName !== "darwin") return;
  const appPath = path.join(context.appOutDir, `${context.packager.appInfo.productFilename}.app`);
  if (!fs.existsSync(appPath)) {
    throw new Error(`[adhoc-sign] app bundle not found: ${appPath}`);
  }
  execFileSync("codesign", ["--force", "--deep", "--sign", "-", appPath], { stdio: "inherit" });
  execFileSync("codesign", ["--verify", "--deep", "--strict", appPath], { stdio: "inherit" });
  console.log(`[adhoc-sign] ad-hoc deep-signed ok: ${appPath}`);
};
