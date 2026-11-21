// extract-archive.mjs — 归档解压（Windows runner 友好）。
//
// 背景（2026-11-20 CI 实测）：GitHub Windows runner 上 `powershell Expand-Archive`
// 依赖 Microsoft.PowerShell.Archive 模块版本——`ExtractToDirectory` 的 3 参数重载
// 在 Windows PowerShell 5.1 与 PowerShell 7 之间不兼容，会抛
// "Cannot find an overload for ExtractToDirectory and the argument count: 3"，
// 解压中断后上层报的是莫名其妙的 ENOENT（找不到解压目录），桌面打包整包中止。
//
// 改用系统自带 bsdtar（%SystemRoot%\System32\tar.exe，Win10+ 内置，支持 zip 与
// tar.gz），Windows 分支彻底不依赖 PowerShell；其他平台沿用 unzip / tar。
import fs from "node:fs";
import path from "node:path";
import { execFileSync } from "node:child_process";

// 系统 bsdtar 路径：Windows 才有（macOS/Linux 的 /usr/bin/tar 是 bsdtar/GNU tar，
// 行为正常，不需要特殊处理）。
function systemBsdTar() {
  if (process.platform !== "win32") return "";
  const root = process.env.SystemRoot || "C:\\Windows";
  return path.join(root, "System32", "tar.exe");
}

// extractZip — 解 zip 到 destDir（不存在则创建）。
export function extractZip(archive, destDir) {
  fs.mkdirSync(destDir, { recursive: true });
  if (process.platform === "win32") {
    const tar = systemBsdTar();
    if (!fs.existsSync(tar)) {
      throw new Error(`extract: 找不到系统 bsdtar（${tar}）`);
    }
    execFileSync(tar, ["-xf", archive, "-C", destDir], { stdio: "inherit" });
    return;
  }
  execFileSync("unzip", ["-q", archive, "-d", destDir], { stdio: "inherit" });
}

// extractTarGz — 解 tar.gz 到 destDir（不存在则创建）。
export function extractTarGz(archive, destDir) {
  fs.mkdirSync(destDir, { recursive: true });
  execFileSync("tar", ["-xzf", archive, "-C", destDir], { stdio: "inherit" });
}
