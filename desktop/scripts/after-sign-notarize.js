/**
 * afterSign：Developer ID 签名完成后提交 Apple 公证并 staple。
 *
 * 不使用 @electron/notarize 的 `submit --wait`：它没有等待上限，也不持续输出
 * submission 状态。这里提交后立即打印 submission ID，每 30 秒查询一次；超时后
 * Apple 仍会继续处理，日志里的 ID 可用于 `notarytool info/log` 恢复查询。
 *
 * 未提供 APPLE_API_KEY 时明确跳过（本地开发与手动 CI）；提供后必须同时提供
 * APPLE_API_KEY_ID / APPLE_API_ISSUER，缺任一项直接失败。
 */
"use strict";

const { execFileSync, spawnSync } = require("node:child_process");
const fs = require("node:fs");
const os = require("node:os");
const path = require("node:path");

const POLL_INTERVAL_MS = 30_000;
const DEFAULT_TIMEOUT_MINUTES = 60;

function sleep(milliseconds) {
  return new Promise((resolve) => setTimeout(resolve, milliseconds));
}

function notaryCredentials() {
  return {
    key: process.env.APPLE_API_KEY,
    keyId: process.env.APPLE_API_KEY_ID,
    issuer: process.env.APPLE_API_ISSUER,
  };
}

function authorizationArgs(credentials) {
  return [
    "--key", credentials.key,
    "--key-id", credentials.keyId,
    "--issuer", credentials.issuer,
  ];
}

function notarytool(args, credentials) {
  const result = spawnSync("xcrun", [
    "notarytool",
    ...args,
    ...authorizationArgs(credentials),
    "--output-format", "json",
  ], {
    encoding: "utf8",
    maxBuffer: 16 * 1024 * 1024,
  });
  if (result.error) throw result.error;
  const stdout = (result.stdout || "").trim();
  const stderr = (result.stderr || "").trim();
  if (result.status !== 0) {
    throw new Error(`notarytool ${args[0]} 失败（exit ${result.status}）：\n${stdout}\n${stderr}`);
  }
  try {
    return JSON.parse(stdout);
  } catch {
    throw new Error(`notarytool ${args[0]} 返回了非 JSON 内容：\n${stdout}\n${stderr}`);
  }
}

function timeoutMinutes() {
  const raw = process.env.AIC_NOTARY_TIMEOUT_MINUTES || String(DEFAULT_TIMEOUT_MINUTES);
  const value = Number(raw);
  if (!Number.isFinite(value) || value <= 0) {
    throw new Error(`AIC_NOTARY_TIMEOUT_MINUTES 必须是正数，当前为 ${raw}`);
  }
  return value;
}

module.exports = async (context) => {
  if (context.electronPlatformName !== "darwin") return;
  const credentials = notaryCredentials();
  if (!credentials.key) {
    console.log("[notary] 未提供 APPLE_API_KEY，跳过公证");
    return;
  }
  if (!credentials.keyId || !credentials.issuer) {
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

    const submitted = notarytool(["submit", archive], credentials);
    if (!submitted.id) throw new Error("[notary] 提交成功但响应缺少 submission ID");
    const id = submitted.id;
    console.log(`[notary] submission ID: ${id}`);

    const limit = timeoutMinutes();
    const deadline = Date.now() + limit * 60_000;
    for (;;) {
      const info = notarytool(["info", id], credentials);
      console.log(`[notary] ${id}: ${info.status}`);
      if (info.status === "Accepted") break;
      if (info.status === "Invalid") {
        const diagnostics = notarytool(["log", id], credentials);
        throw new Error(`[notary] Apple 拒绝公证：\n${JSON.stringify(diagnostics, null, 2)}`);
      }
      if (info.status !== "In Progress") {
        throw new Error(`[notary] 未知状态 ${info.status}：\n${JSON.stringify(info, null, 2)}`);
      }
      if (Date.now() >= deadline) {
        throw new Error(
          `[notary] 等待超过 ${limit} 分钟；Apple 仍会继续处理。\n` +
          `submission ID: ${id}\n` +
          `恢复查询：xcrun notarytool info ${id} --key <p8> --key-id <id> --issuer <issuer>`,
        );
      }
      await sleep(POLL_INTERVAL_MS);
    }

    execFileSync("xcrun", ["stapler", "staple", appPath], { stdio: "inherit" });
    execFileSync("xcrun", ["stapler", "validate", appPath], { stdio: "inherit" });
    console.log(`[notary] 公证与 staple 完成：${id}`);
  } finally {
    fs.rmSync(temporary, { recursive: true, force: true });
  }
};
