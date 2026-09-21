//go:build darwin

package fsauth

// tempRoots（darwin）：平台临时区字面根 /private/tmp（/tmp 的 canonical 形态）。
// os.TempDir()（$TMPDIR）由 rebuildBaseRootsLocked 平台无关段统一收录；
// /tmp 是不写 $TMPDIR 的进程的通用临时回落位置。v0.14.5 统一权限模型时
// 随 exec_procs.writableRoots 下线而丢失（seatbelt subpath 白名单只剩
// $TMPDIR，/tmp 与 /var/tmp 写入 EPERM），2026-09-21 补回——fs Decide 与
// 沙箱 bind 共用同一份名单；darwin seatbelt 纯规则放行无挂载，字面根零副作用。
func tempRoots() []string { return []string{"/private/tmp"} }
