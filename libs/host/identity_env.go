package host

import (
	"os"
	"strconv"
)

// hostIdentityBaseEnv 把设备真实身份注入 vsh 基础环境（Defaults < BaseEnv <
// 请求 Env，引擎未设 BaseEnv 时身份回落到 vsh 虚拟默认 1000:1000）。
//
// 背景：interp 的 cd / test -rwx 等内建走 access()，用虚拟 euid/egid 对照
// 文件真实属主位判权限档；虚拟 1000 与设备真实属主（macOS 501 等）不等时
// 一律落到 other 档，属主专有的 700 目录会被误判「Permission denied」，
// 而 OS 层读写（进程本身就是属主）却正常——表现为「ls/cat 可以、cd 不行」。
// 注入真实身份后虚拟判定与 OS 行为一致。
//
// Windows 的 os.Getuid 返回 -1，跳过即可：fileOwnerIDs 在 Windows 取不到
// 属主时回落 owner=current，判定自洽，无需注入。
func hostIdentityBaseEnv() map[string]string {
	env := make(map[string]string, 4)
	if uid := os.Getuid(); uid >= 0 {
		s := strconv.Itoa(uid)
		env["UID"] = s
		env["EUID"] = s
	}
	if gid := os.Getgid(); gid >= 0 {
		s := strconv.Itoa(gid)
		env["GID"] = s
		env["EGID"] = s
	}
	return env
}
