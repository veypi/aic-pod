/**
 * Apple 公证的共享小工具：凭据、notarytool JSON 调用、带超时的状态轮询。
 *
 * 被 after-sign-notarize.js（构建期）与 notary-wait.js（构建后补 staple）共用。
 * 认证固定走 App Store Connect API Key：
 *   APPLE_API_KEY(.p8 路径) + APPLE_API_KEY_ID + APPLE_API_ISSUER
 */
"use strict";

const { spawnSync } = require("node:child_process");

const POLL_INTERVAL_MS = 30_000;

function credentials() {
  return {
    key: process.env.APPLE_API_KEY,
    keyId: process.env.APPLE_API_KEY_ID,
    issuer: process.env.APPLE_API_ISSUER,
  };
}

function requireCredentials() {
  const value = credentials();
  const missing = [];
  if (!value.key) missing.push("APPLE_API_KEY");
  if (!value.keyId) missing.push("APPLE_API_KEY_ID");
  if (!value.issuer) missing.push("APPLE_API_ISSUER");
  if (missing.length > 0) {
    throw new Error(`[notary] 缺少公证凭据环境变量：${missing.join("、")}`);
  }
  return value;
}

function authorizationArgs(credential) {
  return [
    "--key", credential.key,
    "--key-id", credential.keyId,
    "--issuer", credential.issuer,
  ];
}

function notarytool(args, credential) {
  const result = spawnSync("xcrun", [
    "notarytool",
    ...args,
    ...authorizationArgs(credential),
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

function sleep(milliseconds) {
  return new Promise((resolve) => setTimeout(resolve, milliseconds));
}

function parseTimeoutMinutes(raw, fallback) {
  const value = Number(raw || String(fallback));
  if (!Number.isFinite(value) || value <= 0) {
    throw new Error(`公证等待上限必须是正数（分钟），当前为 ${raw}`);
  }
  return value;
}

/**
 * 轮询直到 Apple 接受。
 * 返回 info 表示 Accepted；返回 null 表示到达等待上限（Apple 仍会继续处理）。
 * Invalid 与未知状态直接抛错；Invalid 会附带 Apple 的详细日志。
 */
async function waitForAcceptance(id, credential, options) {
  const timeoutMinutes = options.timeoutMinutes;
  const log = options.log || console.log;
  const deadline = Date.now() + timeoutMinutes * 60_000;
  for (;;) {
    const info = notarytool(["info", id], credential);
    log(`[notary] ${id}: ${info.status}`);
    if (info.status === "Accepted") return info;
    if (info.status === "Invalid") {
      const diagnostics = notarytool(["log", id], credential);
      throw new Error(`[notary] Apple 拒绝公证：\n${JSON.stringify(diagnostics, null, 2)}`);
    }
    if (info.status !== "In Progress") {
      throw new Error(`[notary] 未知状态 ${info.status}：\n${JSON.stringify(info, null, 2)}`);
    }
    if (Date.now() >= deadline) return null;
    await sleep(POLL_INTERVAL_MS);
  }
}

module.exports = {
  POLL_INTERVAL_MS,
  credentials,
  requireCredentials,
  notarytool,
  parseTimeoutMinutes,
  waitForAcceptance,
};
