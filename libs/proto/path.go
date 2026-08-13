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
	drive := isDrivePath(p)
	if drive {
		// Windows 盘符路径归一为斜杠（vcore 三端公共规范形，执行层 OSVFS
		// 在 OS 边界转换）。path.Clean 不识别反斜杠，不归一下文的根保护等
		// 等值比较会在 C:\ 与 C:/ 两形之间永远失配。
		p = strings.ReplaceAll(p, `\`, "/")
	}
	if path.IsAbs(p) || drive {
		return path.Clean(p), nil
	}
	if workdir == "" {
		return "", fmt.Errorf("proto: relative path %q requires workdir", p)
	}
	if !path.IsAbs(workdir) && !isDrivePath(workdir) {
		return "", fmt.Errorf("proto: workdir must be absolute, got %q", workdir)
	}
	if isDrivePath(workdir) {
		workdir = strings.ReplaceAll(workdir, `\`, "/")
	}
	return path.Clean(path.Join(workdir, p)), nil
}

var driveRe = regexp.MustCompile(`^[A-Za-z]:[\\/]`)

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
