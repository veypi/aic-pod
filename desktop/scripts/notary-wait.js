#!/usr/bin/env node
/**
 * 查询 / 等待一个 Apple 公证 submission。
 *
 * 本机用法（p8 与 issuer 见 signing/README 归档）：
 *   APPLE_API_KEY=signing/ios/AuthKey_xxx.p8 \
 *   APPLE_API_KEY_ID=xxx APPLE_API_ISSUER=xxx \
 *   node desktop/scripts/notary-wait.js --id <submission-id> --once
 *
 * CI finalize 用法：去掉 --once，--timeout-minutes 300 轮询到 Accepted；
 * 超时退出码 1，可用同一 submission ID 重跑，Apple 侧任务不会丢。
 */
"use strict";

const {
  requireCredentials,
  notarytool,
  parseTimeoutMinutes,
  waitForAcceptance,
} = require("./notary-lib");

function parseArgs(argv) {
  const options = { once: false, timeoutMinutes: 300 };
  for (let index = 0; index < argv.length; index += 1) {
    const arg = argv[index];
    if (arg === "--once") options.once = true;
    else if (arg === "--id") options.id = argv[++index];
    else if (arg === "--timeout-minutes") options.timeoutMinutes = argv[++index];
    else if (arg === "--history") options.history = true;
    else throw new Error(`未知参数：${arg}`);
  }
  return options;
}

async function main() {
  const options = parseArgs(process.argv.slice(2));
  const credential = requireCredentials();
  if (options.history) {
    const history = notarytool(["history"], credential);
    console.log(JSON.stringify(history, null, 2));
    return;
  }
  if (!options.id) throw new Error("缺少 --id <submission-id>（或用 --history 列全部）");
  if (options.once) {
    console.log(JSON.stringify(notarytool(["info", options.id], credential), null, 2));
    return;
  }
  const limit = parseTimeoutMinutes(options.timeoutMinutes, 300);
  const accepted = await waitForAcceptance(options.id, credential, { timeoutMinutes: limit });
  if (!accepted) {
    console.error(`[notary] ${limit} 分钟内仍未被接受；可用同一 ID 重跑继续等待。`);
    process.exitCode = 1;
    return;
  }
  console.log(`[notary] 已被 Apple 接受：${options.id}`);
}

main().catch((error) => {
  console.error(error.message || error);
  process.exitCode = 1;
});
