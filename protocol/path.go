package protocol

import (
	"fmt"
	"path"
	"regexp"
	"strings"
)

// ResolvePath 实现 §2.1.1 可解析层的路径展开：纯路径运算，执行前完成。
// 规则匹配层与执行层各自独立调用、结果一致——双端禁止各自另写展开逻辑。
//
//   - workdir：当次调用显式携带的基准目录，必须绝对（缺省值由调用方先行填充：
//     cloud/page = "/"（空间根），物理 host = host 端配置工作区）；
//   - 绝对路径（/ 开头、Windows 盘符）忽略 workdir；
//     其余（含 "."）相对 workdir 展开。
//
// 返回清理后的绝对路径。根收容校验不在此函数——规则匹配层另调 WithinRoots，
// 两处结果一致。
func ResolvePath(p, workdir string) (string, error) {
	if p == "" {
		return "", fmt.Errorf("proto: path is empty")
	}
	// 盘符相关形态归一（主入口，与执行层兜底 OSVFS.winToOS 共用
	// NormalizeHostPath——双端禁止各自另写展开逻辑）。归一后 Windows 路径
	// 为 /c/… 类 Linux 规范形（path.IsAbs 成立），//c、/C: 等形态不存在
	//（根保护等等值比较依赖单一规范形）。
	p = NormalizeHostPath(p)
	if path.IsAbs(p) {
		return p, nil
	}
	if workdir == "" {
		return "", fmt.Errorf("proto: relative path %q requires workdir", p)
	}
	workdir = NormalizeHostPath(workdir)
	if !path.IsAbs(workdir) {
		return "", fmt.Errorf("proto: workdir must be absolute, got %q", workdir)
	}
	return path.Clean(path.Join(workdir, p)), nil
}

// NormalizeHostPath 把 host 路径输入归一为三端公共规范形（纯语法、与 GOOS
// 无关，§2.1.1；2026-09-24 全局统一类 Linux 形，废除 C:/ 盘符规范形）：
//   - Windows 规范形 = /c/…（小写盘符作首段的 unix 绝对路径）：C:\x、C:/x、
//     c:/x、/C:/x、/c/x、//c/x、裸 C: → /c/x 或 /c（裸盘符 = 盘符根）；
//   - 绝对路径先 path.Clean（折叠多斜杠、剥尾斜杠）再判盘符形——规则层
//     根保护等值比较与执行层共用此单一结果；
//   - 非盘符形态原样返回（POSIX 路径不做反斜杠替换——\ 是合法文件名字符）。
//
// mac/linux 上 /C:/x 是合法 POSIX 路径但同样被归一为 /c/x——规则匹配层与
// 执行层必须结果一致，归一不能按 GOOS 分叉。ResolvePath 主入口与 host
// 执行层兜底（winToOS）共用本函数。
func NormalizeHostPath(p string) string {
	if isDrivePath(p) {
		// 盘符形输入：C:、C:/…、C:\…（任意大小写）。
		rest := strings.ReplaceAll(p[2:], `\`, "/")
		return path.Clean("/" + strings.ToLower(p[:1]) + "/" + rest)
	}
	if path.IsAbs(p) {
		p = path.Clean(p)
		if len(p) > 1 && isDrivePath(p[1:]) {
			// 前导斜杠+盘符形：/C:、/C:/…、/C:\…（前端树路径、递归拼接、
			// 用户裸输入的容错收口）。
			rest := strings.ReplaceAll(p[3:], `\`, "/")
			return path.Clean("/" + strings.ToLower(p[1:2]) + "/" + rest)
		}
	}
	return p
}

// SplitDriveRoot 拆规范形首段盘符：/c → ('c', "", true)；/c/Users →
// ('c', "Users", true)；其余 → false。纯语法（首段为单字母即盘符）——
// 只在 Windows 路径语义上下文使用（libs/host osvfs 与 hostfs 的 win 分支），
// POSIX 主流程不得用本函数区别对待 /c/…。
func SplitDriveRoot(p string) (drive byte, rest string, ok bool) {
	if len(p) >= 2 && p[0] == '/' && isLetter(p[1]) {
		if len(p) == 2 {
			return p[1], "", true
		}
		if p[2] == '/' {
			return p[1], p[3:], true
		}
	}
	return 0, "", false
}

func isLetter(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

// WinTmpToOS 把 /tmp（及子路径）映射为 windows 临时目录原生路径：/tmp →
// tmpDir；/tmp/a/b → tmpDir\a\b。其余输入 ok=false。纯函数（tmpDir 注入，
// 跨平台可测）；osvfs/hostfs 的 win 分支与 HostPathToOS 共用本函数，禁止
// 各处另写映射逻辑。
func WinTmpToOS(p, tmpDir string) (string, bool) {
	if p != "/tmp" && !strings.HasPrefix(p, "/tmp/") {
		return "", false
	}
	rest := strings.TrimPrefix(p[len("/tmp"):], "/")
	tmpDir = strings.TrimSuffix(tmpDir, `\`)
	if rest == "" {
		return tmpDir, true
	}
	return tmpDir + `\` + strings.ReplaceAll(rest, "/", `\`), true
}

// driveRe 匹配盘符输入形态：C:（裸盘符 = 盘符根）、C:/…、C:\…。
// C:foo（盘符相对形态）不匹配，保持相对路径语义。
var driveRe = regexp.MustCompile(`^[A-Za-z]:([\\/]|$)`)

func isDrivePath(p string) bool { return driveRe.MatchString(p) }

// WithinRoots 校验展开后的绝对路径是否落在任一根内（§2.1.1 路径空间收容：
// cloud 三根 / page 双根）。symlink 真实路径校验由执行层各自完成。
func WithinRoots(absPath string, roots []string) bool {
	absPath = path.Clean(absPath)
	for _, r := range roots {
		r = path.Clean(r)
		if absPath == r || strings.HasPrefix(absPath, r+"/") {
			return true
		}
	}
	return false
}
