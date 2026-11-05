/**
 * after-pack-sign.js — electron-builder afterPack 钩子：macOS 签名准备 + 随包资源校验
 *
 * 两条路径（按可用证书自动分流，同一份代码）：
 *
 * 1) 有可用的 Developer ID Application 身份（钥匙串里的有效 identity，或 CI 用
 *    CSC_LINK 指定）：
 *    - 先给 Contents/Resources 下我们自带的裸 Mach-O 补签。osx-sign 只签 app 本体、
 *      Frameworks 与 helper，**不签 Resources 里的裸可执行文件**，而公证要求
 *      嵌套代码全部有有效签名，否则 Apple 直接拒收。
 *    - 外层 .app 的签名与公证随后由 electron-builder 用同一证书完成
 *      （见 electron-builder.yml 的 hardenedRuntime / entitlements / notarize）。
 *
 * 2) 没有证书（本地开发、CI 未配凭据）：保留 ad-hoc 深签兜底。下载打开提示从
 *    「已损坏」（需 xattr 清隔离）变为「未验证开发者」（系统设置 → 隐私与安全性 →
 *    仍要打开）。直接 identity: "-" 会踩 electron-builder 26 的 ad-hoc 相机/麦克风
 *    失效坑（electron-builder#9529），故用打包后深签。
 *
 * 顺序要点：afterPack 在 electron-builder 外层签名之前运行——嵌套代码必须先签好，
 * 否则外层封装的是未签名内容，签名校验与公证都会失败。
 *
 * 临时钥匙串按 electron-builder 的做法建（app-builder-lib codeSign/macCodeSign.js）：
 * 完整路径 + 随机钥匙串口令 + set-keychain-settings（不自动锁）+ **加入用户钥匙串
 * 搜索列表**（否则 codesign 解析不到签名者的证书链：报 “The specified item could not
 * be found in the keychain”）+ 导入后 set-key-partition-list。
 *
 * 第三方已签名的嵌套 app（Chrome for Testing、CuaDriver）整棵跳过，不重签：
 * 重签会破坏上游自己的签名链。
 */
"use strict";

const { execFileSync } = require("node:child_process");
const path = require("node:path");
const fs = require("node:fs");
const os = require("node:os");
const crypto = require("node:crypto");
const { Arch } = require("builder-util");

const DEVELOPER_ID_PREFIX = "Developer ID Application:";

// 2025 起 Apple 的 Developer ID 走 G2 签发链（issuer = Developer ID Certification
// Authority / OU=G2），macOS 不预装该中间证书与 G2 根：签名钥匙串里缺它们时，
// codesign 无法为签名者构建证书链（errSecInternalComponent + “unable to build
// chain to self-signed root”）。随包带上这两张公开 CA 证书并导入签名钥匙串。
const G2_CHAIN_CERTS = [
  path.join(__dirname, "..", "build", "certs", "DeveloperIDG2CA.cer"),
  path.join(__dirname, "..", "build", "certs", "AppleRootCA-G2.cer"),
];

// Mach-O 魔数（含大小端两种读法）；fat 二进制同样要签名。
const MACHO_MAGICS = new Set([
  0xfeedface, 0xfeedfacf, 0xcefaedfe, 0xcffaedfe, 0xcafebabe, 0xbebafeca,
]);

function run(command, args) {
  execFileSync(command, args, { stdio: "inherit" });
}

function capture(command, args) {
  try {
    return execFileSync(command, args, { encoding: "utf8", stdio: ["ignore", "pipe", "ignore"] });
  } catch {
    return "";
  }
}

// 用户钥匙串搜索列表（security list-keychains -d user 的输出是带引号的路径列表）
function listUserKeychains() {
  return capture("security", ["list-keychains", "-d", "user"])
    .split("\n")
    .map((line) => line.trim())
    .filter((line) => line.startsWith("\""))
    .map((line) => line.slice(1, -1));
}

// 钥匙串里的有效签名身份。-v 只列未被判定为过期/不受信任的身份
// （与 @electron/osx-sign 的选择口径一致：那里也用 find-identity -v）。
function findValidDeveloperId(keychain) {
  const args = ["find-identity", "-v", "-p", "codesigning"];
  if (keychain) args.push(keychain);
  for (const line of capture("security", args).split("\n")) {
    const match = line.match(/^\s*\d+\) [0-9A-F]+\s+"(.+)"$/);
    if (match && match[1].startsWith(DEVELOPER_ID_PREFIX)) return match[1];
  }
  return null;
}

// CI：CSC_LINK 是 p12 的 base64 或文件路径；导入临时钥匙串供本次构建使用。
function importCscLink() {
  const link = process.env.CSC_LINK;
  if (!link) return null;
  const directory = fs.mkdtempSync(path.join(os.tmpdir(), "aic-sign-"));
  const p12 = path.join(directory, "identity.p12");
  if (fs.existsSync(link)) fs.copyFileSync(link, p12);
  else fs.writeFileSync(p12, Buffer.from(link, "base64"));
  const password = process.env.CSC_KEY_PASSWORD || "";
  const keychain = path.join(directory, "aic-sign.keychain");
  const keychainPassword = crypto.randomBytes(32).toString("base64");
  run("security", ["create-keychain", "-p", keychainPassword, keychain]);
  run("security", ["unlock-keychain", "-p", keychainPassword, keychain]);
  run("security", ["set-keychain-settings", keychain]);
  // 必须进搜索列表：否则 codesign 解析不到签名者的证书链。
  const previous = listUserKeychains();
  run("security", ["list-keychains", "-d", "user", "-s", keychain, ...previous]);
  run("security", ["import", p12, "-k", keychain, "-T", "/usr/bin/codesign", "-T", "/usr/bin/productbuild", "-P", password]);
  // 把 G2 链导入同一钥匙串：签名者（Developer ID 证书）的签发者是 G2 中间证书。
  for (const ca of G2_CHAIN_CERTS) {
    if (fs.existsSync(ca)) run("security", ["import", ca, "-k", keychain]);
  }
  // 不设分区列表，CI 里 codesign 会因密钥 ACL 报 “User interaction is not allowed”。
  // 分区列表与 electron-builder 自身导入 CSC_LINK 时保持一致（见 app-builder-lib macCodeSign）。
  // 注意 -k 传的是**钥匙串密码**（不是 p12 口令）：传错会报 “passphrase not correct”。
  try {
    run("security", ["set-key-partition-list", "-S", "apple-tool:,apple:", "-s", "-k", keychainPassword, keychain]);
  } catch {
    console.warn("[sign] set-key-partition-list 失败，继续");
  }
  return { keychain, previous };
}

function resolveIdentity() {
  // CI 已在独立步骤里把 p12 导入搜索列表内的钥匙串，此时直接用搜索列表里的有效身份。
  if (!process.env.CSC_LINK) {
    const identity = findValidDeveloperId(null);
    return { identity, keychain: null, temporary: null };
  }
  const temporary = importCscLink();
  const keychain = temporary ? temporary.keychain : null;
  let identity = findValidDeveloperId(keychain);
  if (!identity && process.env.CSC_NAME) {
    const valid = capture("security", ["find-identity", "-v", "-p", "codesigning"]);
    if (valid.includes(process.env.CSC_NAME)) identity = process.env.CSC_NAME;
  }
  return { identity, keychain, temporary };
}

function isMachO(file) {
  const descriptor = fs.openSync(file, "r");
  try {
    const header = Buffer.alloc(4);
    if (fs.readSync(descriptor, header, 0, 4, 0) < 4) return false;
    return MACHO_MAGICS.has(header.readUInt32BE(0));
  } finally {
    fs.closeSync(descriptor);
  }
}

// 收集 Resources 下的裸 Mach-O；.app / .framework 目录整棵跳过（上游已签名）。
function collectNestedBinaries(directory, found = []) {
  for (const entry of fs.readdirSync(directory, { withFileTypes: true })) {
    const full = path.join(directory, entry.name);
    if (entry.isDirectory()) {
      if (entry.name.endsWith(".app") || entry.name.endsWith(".framework")) continue;
      collectNestedBinaries(full, found);
    } else if (entry.isFile() && isMachO(full)) {
      found.push(full);
    }
  }
  return found;
}

function signNestedBinaries(binaries, identity, keychain) {
  for (const file of binaries) {
    const args = ["--force", "--sign", identity, "--options", "runtime", "--timestamp"];
    if (keychain) args.push("--keychain", keychain);
    args.push(file);
    run("codesign", args);
    run("codesign", ["--verify", "--strict", file]);
  }
}

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
  if (!fs.existsSync(appPath)) throw new Error(`[sign] app bundle not found: ${appPath}`);

  const { identity, keychain, temporary } = resolveIdentity();
  try {
    if (identity) {
      const nested = collectNestedBinaries(resources);
      if (!nested.length) throw new Error("[sign] 未找到随包 Mach-O，签名流水线与预期不符");
      signNestedBinaries(nested, identity, keychain);
      console.log(`[sign] nested binaries signed (${nested.length}) with ${identity}`);
      return;
    }
    execFileSync("codesign", ["--force", "--deep", "--sign", "-", appPath], { stdio: "inherit" });
    execFileSync("codesign", ["--verify", "--deep", "--strict", appPath], { stdio: "inherit" });
    console.log(`[sign] ad-hoc deep-signed (no valid Developer ID identity): ${appPath}`);
  } finally {
    if (temporary) {
      try {
        run("security", ["list-keychains", "-d", "user", "-s", ...temporary.previous]);
        run("security", ["delete-keychain", temporary.keychain]);
      } catch {
        console.warn(`[sign] 临时钥匙串清理失败：${temporary.keychain}`);
      }
    }
  }
};
