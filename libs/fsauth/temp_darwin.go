//go:build darwin

package fsauth

// tempRoots（darwin）：平台临时区根。两个拼写都要在名单里——沙箱按系统调用
// 实际传入的路径串匹配规则，/tmp 是指向 /private/tmp 的 symlink，只列 canonical
// 形会让字面 /tmp（含路径解析的 metadata 读）被拒（2026-09-22）。
func tempRoots() []string { return []string{"/private/tmp", "/tmp"} }
