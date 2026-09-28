package proto

import "strings"

// 可写根前缀判定与写分级（v0.14.5 统一文件权限模型的跨端单源）：
// cloud（aic GatedFS/CloudWriteGradePaths）与 host（aic-pod fsauth.Policy）共用，
// 两侧禁止再各自实现前缀匹配（此前 aic inWriteRoots 与 fsauth decide 各一份）。

// InWriteRoots 判定 name 是否命中可写根集合（等于某根或位于其下）。
// 根为空串跳过；两侧尾斜杠归一后匹配。
func InWriteRoots(name string, roots []string) bool {
	name = strings.TrimSuffix(name, "/")
	for _, r := range roots {
		if r == "" {
			continue
		}
		r = strings.TrimSuffix(r, "/")
		if name == r || strings.HasPrefix(name, r+"/") {
			return true
		}
	}
	return false
}
