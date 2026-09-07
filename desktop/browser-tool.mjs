/**
 * browser-tool.js — desktop 的 browser 命令装配与壳通道服务（ESM，main.js 动态 import）
 *
 * 链路：平台 → Go 后端（NATS exec 请求）→ provider 转发（TCP 换行 JSON）→
 *   本模块 → browser core（../browser 共享代码）→ electron-adapter（CDP）。
 *
 *   - 通道：127.0.0.1 随机端口 TCP，每请求一行 JSON（见 Go libs/host/register.go
 *     的 ShellRequest/ShellResponse）；token 由本模块生成、注册时交 Go 回带校验
 *     （防本机其他进程伪造调用）。
 *   - 串行链：browser 全部命令串行执行（@ref 代次状态机 + 共享工作区窗口，
 *     对齐插件端 client.chains 的 stateful 串行语义）。
 *   - 文件产物：截图/下载落 Go 下发的会话工作区（session_dir），AI 经 host fs 读。
 */

import net from "node:net";
import crypto from "node:crypto";
import fs from "node:fs";
import path from "node:path";
import os from "node:os";

import { createBrowserHandler } from "./vendor/browser/tools/browser/core.js";
import { createElectronAdapter } from "./electron-adapter.js";

// makeFs：core 的 ctx.fs 落盘适配——/screenshot/* 与 /browser/* 前缀映射到
// 会话工作区的 .screenshot/.browser 目录（host fs 会话区可读）。
function makeFs(sessionDir) {
  return {
    async put(p, blob) {
      const name = path.basename(String(p));
      const sub = String(p).startsWith("/screenshot/") ? ".screenshot" : ".browser";
      const dir = path.join(sessionDir, sub);
      fs.mkdirSync(dir, { recursive: true, mode: 0o700 });
      const buf = Buffer.from(await blob.arrayBuffer());
      const full = path.join(dir, name);
      fs.writeFileSync(full, buf, { mode: 0o600 });
      return { path: full, bytes: buf.length };
    },
  };
}

// sessionDir 防线：仅接受 $HOME/.aic/sessions/ 内的路径（Go sessionWorkDir 的
// 产物；防伪造请求把文件写到任意目录）。
function safeSessionDir(dir) {
  const root = path.join(os.homedir(), ".aic", "sessions") + path.sep;
  const resolved = path.resolve(String(dir || ""));
  if (!resolved.startsWith(root)) return null;
  return resolved;
}

export async function startBrowserServer({ log } = {}) {
  const logf = log || (() => {});
  const adapter = createElectronAdapter();
  const handler = createBrowserHandler(adapter);
  const token = crypto.randomBytes(16).toString("hex");

  // browser 命令串行链（stateful 语义：并发 click/snapshot 会互毁 @ref 代次）
  let chain = Promise.resolve();

  function handleLine(line, conn) {
    const reply = (o) => {
      try {
        conn.end(JSON.stringify(o) + "\n");
      } catch { /* 连接已断 */ }
    };
    let req;
    try {
      req = JSON.parse(line);
    } catch {
      return reply({ state: "error", error: "invalid json" });
    }
    if (req.token !== token) return reply({ state: "error", error: "invalid token" });
    const sessionDir = safeSessionDir(req.session_dir);
    if (!sessionDir) return reply({ state: "error", error: "invalid session_dir" });

    chain = chain.then(async () => {
      adapter.setSessionDir(sessionDir);
      const ctx = {
        grantedLevel: req.granted_level,
        sessionID: req.session_id,
        msgID: req.msg_id,
        fs: makeFs(sessionDir),
      };
      try {
        const res = await handler(ctx, { argv: req.argv });
        reply({
          state: res.state || "completed",
          content: res.content || "",
          error: res.error || "",
          attrs: res.attrs || {},
        });
      } catch (e) {
        reply({ state: "error", error: String(e?.message || e) });
      }
    });
    // 链上错误不应毒化后续请求
    chain = chain.catch((e) => logf("[browser] chain error: %s", e?.message || e));
  }

  const server = net.createServer((conn) => {
    let buf = "";
    conn.on("data", (d) => {
      buf += d;
      // 防线：单行最大 4MB（eval 大脚本/大结果），防内存放大
      if (buf.length > 4 * 1024 * 1024) {
        conn.destroy();
        return;
      }
      let i;
      while ((i = buf.indexOf("\n")) >= 0) {
        const line = buf.slice(0, i).trim();
        buf = buf.slice(i + 1);
        if (line) handleLine(line, conn);
      }
    });
    conn.on("error", () => {});
  });

  await new Promise((resolve, reject) => {
    server.once("error", reject);
    server.listen(0, "127.0.0.1", resolve);
  });
  const port = server.address().port;
  logf("[browser] shell channel listening on 127.0.0.1:%d", port);
  return { port, token, server };
}
