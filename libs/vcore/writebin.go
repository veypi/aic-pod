package vcore

import (
	"path"
)

// MaxWriteBinBytes 是 WriteBin 单次写入的字节上限（256MB，与 ReadBin 对称）：
// writebin 为整文件驻内存通道，超限明确报错。
const MaxWriteBinBytes = 256 << 20

// WriteBin 把原始字节整写入文件（2026-09-12，RTC 直连通道 writebin op 的
// 执行体）：**不属于 fs 指令集**（8 action 契约不变，AI 面不可见），仅经
// owner 直连控制台暴露——host fs put 二进制内容（PNG/zip 等）的字节入口，
// 与 ReadBin（读出口）对称。
//
// 权限门与 fs write 同一判定实例：Resolve → CheckPath → CheckPolicy（write
// 级，deny 恒拒不可绕过）。父目录不存在自动创建（MkdirAll 0o755，与 fs write
// 一致），整文件覆写 0o644。返回写入字节数。
func WriteBin(env *Env, filePath string, data []byte) (int, error) {
	if filePath == "" {
		return 0, fsErr("writebin", "path is required")
	}
	if data == nil {
		data = []byte{}
	}
	if len(data) > MaxWriteBinBytes {
		return 0, fsErr("writebin", "%d bytes exceeds max %d", len(data), MaxWriteBinBytes)
	}
	abs, err := env.Resolve(filePath)
	if err != nil {
		return 0, fsErr("writebin", "%s", err)
	}
	if err := env.CheckPath("writebin", abs); err != nil {
		return 0, err
	}
	if err := env.CheckPolicy("writebin", abs, true); err != nil {
		return 0, err
	}
	if err := env.VFS.MkdirAll(path.Dir(abs), 0o755); err != nil {
		return 0, fsVFSErr("writebin", err, "%s", err)
	}
	if err := env.VFS.WriteFile(abs, data, 0o644); err != nil {
		return 0, fsVFSErr("writebin", err, "%s", err)
	}
	return len(data), nil
}
