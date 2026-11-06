/**
 * afterSign：Developer ID 签名完成后提交 Apple 公证并 staple。
 *
 * 不使用 @electron/notarize 的 `submit --wait`：它没有等待上限，也不持续输出
 * submission 状态。这里提交后立即打印 submission ID，每 30 秒查询一次。
 *
 * 超时策略（实测 Apple 队列可能耗时 1～13 小时，包本身最终都会被接受）：
 *   默认等待 4 小时；仍未接受时**不判失败**，而是把已签名的 .app zip 与
 *   submission ID 落到 dist/notary-pending/，由 CI 上传为 signed-app-* artifact。
 *   之后用 notarize-finalize 工作流继续等 Apple、staple 并重打 DMG，不必重新构建。
 *
 * 未提供 APPLE_API_KEY 时明确跳过（本地开发与手动 CI）；提供后必须同时提供
 * APPLE_API_KEY_ID / APPLE_API_ISSUER，缺任一项直接失败。
 */
"use strict";

const { execFileSync } = require("node:child_process");
const fs = require("node:fs");
const os = require("node:os");
const path = require("node:path");
const { Arch } = require("builder-util");
const {
  credentials,
  notarytool,
  parseTimeoutMinutes,
  waitForAcceptance,
} = require("./notary-lib");

const DEFAULT_TIMEOUT_MINUTES = 240;

function preservePending(context, archive, id) {
  const arch = Arch[context.arch];
  const pendingDir = path.join(context.outDir, "notary-pending");
  const zipName = `notary-pending-${arch}.app.zip`;
  fs.mkdirSync(pendingDir, { recursive: true });
  fs.copyFileSync(archive, path.join(pendingDir, zipName));
  const metadata = {
    submissionId: id,
    arch,
    appZip: zipName,
    version: context.packager.appInfo.version,
    submittedAt: new Date().toISOString(),
  };
  fs.writeFileSync(
    path.join(pendingDir, `notary-pending-${arch}.json`),
    `${JSON.stringify(metadata, null, 2)}\n`,
  );
  console.log(
    `[notary] 已保留签名后的 app（${zipName}），本次不打 stapled DMG。\n` +
    `[notary] 后续用 notarize-finalize 工作流继续等待并补 staple，submission ID: ${id}`,
  );
}

module.exports = async (context) => {
  if (context.electronPlatformName !== "darwin") return;
  const credential = credentials();
  if (!credential.key) {
    console.log("[notary] 未提供 APPLE_API_KEY，跳过公证");
    return;
  }
  if (!credential.keyId || !credential.issuer) {
    throw new Error("[notary] APPLE_API_KEY_ID 与 APPLE_API_ISSUER 必须同时配置");
  }

  const appPath = path.join(
    context.appOutDir,
    `${context.packager.appInfo.productFilename}.app`,
  );
  const temporary = fs.mkdtempSync(path.join(os.tmpdir(), "aic-notary-"));
  const archive = path.join(temporary, "AIC Desktop.zip");
  try {
    execFileSync("ditto", [
      "-c", "-k", "--sequesterRsrc", "--keepParent",
      path.basename(appPath), archive,
    ], {
      cwd: path.dirname(appPath),
      stdio: "inherit",
    });

    const submitted = notarytool(["submit", archive], credential);
    if (!submitted.id) throw new Error("[notary] 提交成功但响应缺少 submission ID");
    const id = submitted.id;
    console.log(`[notary] submission ID: ${id}`);

    const limit = parseTimeoutMinutes(
      process.env.AIC_NOTARY_TIMEOUT_MINUTES,
      DEFAULT_TIMEOUT_MINUTES,
    );
    const accepted = await waitForAcceptance(id, credential, { timeoutMinutes: limit });
    if (!accepted) {
      console.log(`[notary] 等待超过 ${limit} 分钟；Apple 仍会继续处理，submission ID: ${id}`);
      preservePending(context, archive, id);
      return;
    }

    execFileSync("xcrun", ["stapler", "staple", appPath], { stdio: "inherit" });
    execFileSync("xcrun", ["stapler", "validate", appPath], { stdio: "inherit" });
    console.log(`[notary] 公证与 staple 完成：${id}`);
  } finally {
    fs.rmSync(temporary, { recursive: true, force: true });
  }
};
