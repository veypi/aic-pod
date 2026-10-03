package proto

import (
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

// NormalizeHostPathList 将 OS PATH 转为 shell 使用的规范路径列表。
// 只包含真实目录，不注入命令专用目录。
func NormalizeHostPathList(value string) string {
	paths := filepath.SplitList(value)
	for i, p := range paths {
		if p != "" {
			paths[i] = NormalizeHostPath(filepath.ToSlash(p))
		}
	}
	return strings.Join(paths, ":")
}

// HostEnvMapToOS 将 shell 导出的环境转为 OS 环境，不修改调用方的 map。
// 与 cwd 一样，标准路径变量在进程或 service 协议边界还原，普通值原样传递。
func HostEnvMapToOS(env map[string]string) map[string]string {
	out := make(map[string]string, len(env))
	for key, value := range env {
		switch strings.ToUpper(key) {
		case "PATH":
			paths := strings.Split(value, ":")
			for i, p := range paths {
				if p != "" {
					paths[i] = HostPathToOS(p)
				}
			}
			value = strings.Join(paths, string(os.PathListSeparator))
		case "HOME", "PWD", "OLDPWD", "TMPDIR", "TMP", "TEMP":
			if value != "" {
				value = HostPathToOS(value)
			}
		}
		out[key] = value
	}
	return out
}

// HostEnvToOS 将同一 OS 环境序列化为进程覆盖项。
func HostEnvToOS(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for key, value := range HostEnvMapToOS(env) {
		out = append(out, key+"="+value)
	}
	sort.Strings(out)
	return out
}

// HostEnvFromOS seeds the shell once. Native/process calls subsequently receive
// only their exported Invocation.Env, so env -i and unset remain effective.
func HostEnvFromOS(env []string) map[string]string {
	out := make(map[string]string, len(env))
	for _, kv := range env {
		key, value, ok := strings.Cut(kv, "=")
		if !ok || key == "" {
			continue
		}
		if runtime.GOOS == "windows" {
			switch strings.ToUpper(key) {
			case "PATH", "HOME", "PWD", "OLDPWD", "TMPDIR", "TMP", "TEMP":
				key = strings.ToUpper(key)
			}
		}
		switch key {
		case "PATH":
			value = NormalizeHostPathList(value)
		case "HOME", "PWD", "OLDPWD", "TMPDIR", "TMP", "TEMP":
			if value != "" {
				value = NormalizeHostPath(filepath.ToSlash(value))
			}
		}
		out[key] = value
	}
	return out
}
