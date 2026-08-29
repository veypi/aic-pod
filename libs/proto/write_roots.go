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

// WriteGrade 返回路径集合的写分级：任一路径位于全部根外 → LevelDanger(3)；
// 其余（含空路径集）→ LevelWrite(2)。空名单 = 分级关闭，恒 2（与
// GatedFS.WriteRoots 未配置语义一致——两侧调用方恒传非空名单，此处把空名单
// 语义钉死为「不分级」，消灭历史上 writeRequired 恒 2 / CloudWriteGrade 全 3
// 的相反语义）。
func WriteGrade(paths []string, roots []string) int {
	if len(roots) == 0 {
		return LevelWrite
	}
	for _, p := range paths {
		if p == "" {
			continue
		}
		if !InWriteRoots(p, roots) {
			return LevelDanger
		}
	}
	return LevelWrite
}
