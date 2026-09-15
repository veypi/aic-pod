package proto

import (
	"fmt"
	"path"
	"regexp"
	"strings"
)

// ResolvePath 实现 §2.1.1 可解析层的路径展开：纯路径运算，执行前完成。
// 规则匹配层与执行层各自独立调用、结果一致——双端禁止各自另写展开逻辑。
//
//   - vars：根变量映射（预留；三端当前均无变量），nil 表示该端无变量
//     （物理 host 不做变量展开，§4.1）；
//   - workdir：当次调用显式携带的基准目录，必须绝对（缺省值由调用方先行填充：
//     cloud/page = "/"（空间根），物理 host = host 端配置工作区）；
//   - 绝对路径（/ 开头、根变量开头、Windows 盘符）忽略 workdir；
//     其余（含 "."）相对 workdir 展开。
//
// 返回清理后的绝对路径。根变量路径展开后逃逸变量根 → 错误。
// 根收容校验不在此函数——规则匹配层另调 WithinRoots，两处结果一致。
func ResolvePath(p, workdir string, vars map[string]string) (string, error) {
	if p == "" {
		return "", fmt.Errorf("proto: path is empty")
	}
	// 根变量前缀：最长匹配，变量名后必须跟 / 或结束（"$USERX/a" 不匹配 "$USER"）。
	if len(vars) > 0 && p[0] == '$' {
		name, rest := "", ""
		for k := range vars {
			if len(k) > len(name) && strings.HasPrefix(p, k) &&
				(len(p) == len(k) || p[len(k)] == '/') {
				name, rest = k, p[len(k):]
			}
		}
		if name != "" {
			root := vars[name]
			joined := path.Clean(root + rest)
			if joined != root && !strings.HasPrefix(joined, root+"/") {
				return "", fmt.Errorf("proto: path %q escapes root %s", p, name)
			}
			return joined, nil
		}
		// 未匹配的 $ 开头按字面相对路径处理（不做任何变量展开）。
	}
	// 盘符相关形态归一（主入口，与执行层兜底 OSVFS.winToOS 共用
	// NormalizeDrivePath——双端禁止各自另写展开逻辑）。归一后的绝对路径中
	// //C:、/C: 等形态不存在（根保护等等值比较依赖单一规范形）。
	p = NormalizeDrivePath(p)
	if path.IsAbs(p) || isDrivePath(p) {
		return p, nil
	}
	if workdir == "" {
		return "", fmt.Errorf("proto: relative path %q requires workdir", p)
	}
	if !path.IsAbs(workdir) && !isDrivePath(workdir) {
		return "", fmt.Errorf("proto: workdir must be absolute, got %q", workdir)
	}
	if isDrivePath(workdir) {
		workdir = strings.ToUpper(workdir[:1]) + strings.ReplaceAll(workdir[1:], `\`, "/")
	}
	return path.Clean(path.Join(workdir, p)), nil
}

// NormalizeDrivePath 把盘符相关输入归一为 vcore 三端公共规范形（纯语法、与 GOOS
// 无关，§2.1.1）：
//   - 绝对路径先 path.Clean（折叠多斜杠、剥尾斜杠）再判盘符形——//C:、///C:/x
//     等多斜杠前缀形态归一后不存在（规则层根保护等值比较与执行层共用此结果）；
//   - 前导斜杠+盘符形归一：/C:、/C:/…、/C:\… → 盘符形（host 端输入容错收口：
//     前端树路径 /C:/…、ls/rg 递归拼接产物、用户裸输入统一在此归一）；
//   - 盘符形内反斜杠归一为 /、盘符字母大写、内部多斜杠折叠（C://x → C:/x）；
//     裸 C: = 盘符根。path.Clean 不识别反斜杠、不改大小写，不归一后文的根保护
//     等等值比较会在 C:\ 与 C:/、c: 与 C: 两形之间永远失配；
//   - 非盘符形态原样返回（POSIX 路径不做反斜杠替换——\ 是合法文件名字符）。
//
// mac/linux 上 /C:/x 是合法 POSIX 路径但同样被归一——规则匹配层与执行层必须
// 结果一致，归一不能按 GOOS 分叉。ResolvePath 主入口与 host 执行层兜底
// （winToOS）共用本函数。
func NormalizeDrivePath(p string) string {
	if path.IsAbs(p) {
		p = path.Clean(p)
	}
	if len(p) > 1 && p[0] == '/' && isDrivePath(p[1:]) {
		p = p[1:]
	}
	if !isDrivePath(p) {
		return p
	}
	p = strings.ToUpper(p[:1]) + p[1:]
	p = strings.ReplaceAll(p, `\`, "/")
	return path.Clean(p)
}

// driveRe 匹配盘符路径：C:（裸盘符 = 盘符根）、C:/…、C:\…。
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
