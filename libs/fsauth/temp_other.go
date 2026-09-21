//go:build !darwin

package fsauth

// tempRoots（非 darwin）：无字面临时根——os.TempDir() 由
// rebuildBaseRootsLocked 统一收录已足够：
//   - linux：bwrap 沙箱内 /tmp 已是私有 tmpfs（--tmpfs /tmp，见 bwrapArgs）；
//     字面根进 WriteRootsFor 会被 planConfined 逐项 --bind 宿主目录覆盖
//     tmpfs，破坏私有临时区语义。
//   - windows：沙箱子进程 TMP/TEMP 重定向到 per-call 私有临时目录
//     （sandbox_windows.go），无需字面根。
func tempRoots() []string { return nil }
