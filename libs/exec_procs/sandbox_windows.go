//go:build windows

// Windows 沙箱后端（路径 A）：受限令牌（CreateRestrictedToken）+ ACL 授权，
// host 进程内创建令牌后经 SysProcAttr.Token 注入子进程，无独立 runner。
//
// 机制（与 dsh sandbox-windows-acl / codex windows-sandbox-rs 同模型，
// Go 原生实现）：
//   - 受限令牌：flags = DISABLE_MAX_PRIVILEGE | LUA_TOKEN | WRITE_RESTRICTED，
//     restricting list = logon SID + Everyone + 能力 SID（工作区/私有临时）+
//     per-call deny SID。写类访问做两次检查（正常 SID + restricting SID），
//     两次都通过才放行 —— restricting 列表即进程的写白名单；读不做第二次
//     检查（WRITE_RESTRICTED 只交叉检查写访问，dsh/codex 同边界）。
//   - 能力 SID 是确定性派生（路径哈希）或随机（私有临时目录）的自定义 SID，
//     对应目录的 DACL 上授予完全访问 ACE：
//   - 工作区：standing ACE，幂等授权——每次调用先检查 DACL 是否已有该
//     能力 SID 的完全访问 ACE，有则跳过（不产生重复 ACE）；目录被删重建
//     后 ACE 消失，下次调用自动重新授权（无进程级缓存，无陈旧状态）
//   - 私有临时目录：per-call 随机创建 + 随机 SID，进程结束后删除目录
//     （ACE 随目录消失，无需显式撤销）
//   - read-only：restricting list 无能力 SID → 除 Everyone 可写对象外全部
//     写被拒。
//   - fs deny：per-call 随机 SID 的完全拒绝 ACE（DENY 置于 ACL 首部、继承
//     到子对象；其他进程无该 SID 不受影响）——写被拒（pass-2 命中 deny）；
//     读不在 WRITE_RESTRICTED 检查范围（Windows 边界：读侧隔离需另一机制，
//     与 dsh/codex 一致）；进程结束后撤销。
//   - spawn 控制台策略：保持既有 CREATE_NO_WINDOW（proc_windows.go
//     SetSysProcAttr）——restricting 列表正确时各控制台模式均可启动
//
// （TestWindowsConsoleModeMatrix 真机矩阵）。
//   - TMP/TEMP 环境变量指向私有临时目录（子进程继承）。
package exec_procs

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/veypi/aic-pod/libs/fsauth"
	"github.com/veypi/aic-pod/libs/proto"
)

// ---- x/sys 缺失的 advapi32/kernel32 API（LazyDLL 自封装）----

var (
	advapi32 = windows.NewLazySystemDLL("advapi32.dll")
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")

	// Win10+ 将安全 API 迁移到 API set（api-ms-win-security-*），advapi32 仅
	// 部分转发（实测 Win11 26200：CreateRestrictedToken 在 advapi32）。
	// ACL API（SetEntriesInAcl/SetNamedSecurityInfo）走 x/sys 封装
	// （zsyscall 静态绑定 advapi32.SetEntriesInAclW），不在此手写。
	securityBaseDll = windows.NewLazySystemDLL("api-ms-win-security-base-l1-2-0.dll")

	procCreateRestrictedToken = findProcAny("CreateRestrictedToken", advapi32, kernel32, securityBaseDll)
)

// findProcAny 在多个 DLL 中查找过程（Find 不 panic，Call 才 panic）。
// 全部缺失返回 nil，调用方需判空返回错误。
func findProcAny(name string, dlls ...*windows.LazyDLL) *windows.LazyProc {
	for _, d := range dlls {
		p := d.NewProc(name)
		if err := p.Find(); err == nil {
			return p
		}
	}
	return nil
}

const (
	subContainersAndObjectsInherit = 3 // 子目录 + 文件继承 ACE
	fileAllAccess                  = 0x001F01FF

	// CreateRestrictedToken Flags（dsh sandbox-windows-acl / codex
	// windows-sandbox-rs 同款组合，Win11 26200 实测）：
	//   - DISABLE_MAX_PRIVILEGE：除 SeChangeNotifyPrivilege 外特权全禁；
	//   - LUA_TOKEN：LUA 令牌；
	//   - WRITE_RESTRICTED：restricting SID 仅参与写访问的第二次检查
	//     （pass-2）——写隔离的唯一开关：缺它时 restricting 列表整体惰性
	//     （实测读写均不拦截）。
	// 历史排查结论（2026-09-23 真机矩阵复核）：bfe547d 的 "加 flags 后
	// cmd/powershell/git 全灭 0xC0000142" 根因是 CreateRestrictedToken 参数
	// 错位——restricting SID 数组被传进 PrivilegesToDelete 槽位，列表实际
	// 为空。flag 恢复 + 参数修正后各控制台模式（含 CREATE_NO_WINDOW）在
	// 有无控制台父进程下均正常启动，无需换 spawn 侧控制台策略。
	disableMaxPrivilege = 0x1
	luaToken            = 0x4
	writeRestricted     = 0x8
)

// createRestrictedToken 创建受限令牌（dsh sandbox-windows-acl / codex
// windows-sandbox-rs 同款组合）：restricting list = logon SID + Everyone +
// 能力 SIDs（工作区/私有临时）+ per-call deny SID；flags =
// DISABLE_MAX_PRIVILEGE | LUA_TOKEN | WRITE_RESTRICTED。
// 随后设置宽松默认 DACL：沙箱进程新建对象（pipe/IPC 等）的自身 DACL
// 必须能命中 restricting SID，否则新建即被 pass-2 写检查拒
// （codex/dsh 同策略）；结尾显式启用 SeChangeNotifyPrivilege。
// 现有令牌必须以 TOKEN_ALL_ACCESS 打开——实测 TOKEN_QUERY|TOKEN_DUPLICATE
// 句柄调用 CreateRestrictedToken 返回 Access denied（win11 26200）。
func createRestrictedToken(extraSids []*windows.SID) (windows.Token, error) {
	var procToken windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_ALL_ACCESS, &procToken); err != nil {
		return 0, err
	}
	defer procToken.Close()

	logonSid, err := logonSidOf(procToken)
	if err != nil {
		return 0, err
	}
	everyone, err := windows.CreateWellKnownSid(windows.WinWorldSid)
	if err != nil {
		return 0, err
	}
	// 列表刻意不含用户 SID 与 INTERACTIVE/Authenticated Users/BUILTIN
	// Users：这些身份在宿主上有大量环境写授权（用户自有文件、C:\ 根树、
	// Public 树、WMI 命名空间等），进入 restricting 列表等于把这些写路径
	// 全部放行——写白名单的唯一粒度 = 能力 SID（dsh 同款裁剪）。
	// [logon SID, Everyone] 保底：缺它们在 Win11 下早期 DLL 初始化即
	// STATUS_DLL_INIT_FAILED、CNG 写失败（pwsh 崩溃）——dsh 实测记录。
	restrictions := []windows.SIDAndAttributes{
		{Sid: logonSid},
		{Sid: everyone},
	}
	// 去重：planConfined 的 dirs（工作区/缓存/公共/extra）可能重复出现同一
	// 路径（如 workdir 同时作为 extra），能力 SID 随之重复——restricting
	// 列表保持每个 SID 一条。
	seen := map[string]bool{logonSid.String(): true, everyone.String(): true}
	for _, s := range extraSids {
		if s == nil || seen[s.String()] {
			continue
		}
		seen[s.String()] = true
		restrictions = append(restrictions, windows.SIDAndAttributes{Sid: s})
	}

	var newToken windows.Token
	if procCreateRestrictedToken == nil {
		return 0, procMissing("CreateRestrictedToken")
	}
	// CreateRestrictedToken(Existing, Flags, DisableCount, SidsToDisable,
	//   DeletePrivilegeCount, PrivilegesToDelete, RestrictCount, SidsToRestrict,
	//   NewToken)——注意 PrivilegesToDelete 对在 SidsToRestrict 之前；
	// 参数错位会把 restricting SID 数组当成特权列表传入（空 restricting
	// 列表，写全拒而任何 ACE 都无关——2026-09-23 实测定位）。
	r1, _, e1 := procCreateRestrictedToken.Call(
		uintptr(procToken),
		uintptr(disableMaxPrivilege|luaToken|writeRestricted),
		0, 0, // DisableSidCount, SidsToDisable
		0, 0, // DeletePrivilegeCount, PrivilegesToDelete
		uintptr(len(restrictions)),
		uintptr(unsafe.Pointer(&restrictions[0])),
		uintptr(unsafe.Pointer(&newToken)),
	)
	if r1 == 0 {
		return 0, e1
	}
	if err := setDefaultDacl(newToken, restrictions); err != nil {
		newToken.Close()
		return 0, err
	}
	if err := enableChangeNotify(newToken); err != nil {
		newToken.Close()
		return 0, err
	}
	return newToken, nil
}

// enableChangeNotify 显式启用 SeChangeNotifyPrivilege（目录遍历旁路）。
// 该特权本应被 DISABLE_MAX_PRIVILEGE 保留，显式启用为对齐已实测的参考
// 实现（codex windows-sandbox-rs 同做法），防 LUA 令牌下被禁。
func enableChangeNotify(token windows.Token) error {
	name, err := windows.UTF16PtrFromString("SeChangeNotifyPrivilege")
	if err != nil {
		return err
	}
	var luid windows.LUID
	if err := windows.LookupPrivilegeValue(nil, name, &luid); err != nil {
		return err
	}
	tp := windows.Tokenprivileges{
		PrivilegeCount: 1,
		Privileges: [1]windows.LUIDAndAttributes{{
			Luid:       luid,
			Attributes: windows.SE_PRIVILEGE_ENABLED,
		}},
	}
	return windows.AdjustTokenPrivileges(token, false, &tp, 0, nil, nil)
}

// procMissing 是 win32 过程缺失错误。
func procMissing(name string) error {
	return fmt.Errorf("sandbox: %s not found in advapi32/kernel32", name)
}

// setDefaultDacl 给令牌设置宽松默认 DACL（所有 restricting SID 获得
// 完全访问）：沙箱进程新建对象（pipe/IPC）时无需逐对象授权。
func setDefaultDacl(token windows.Token, sids []windows.SIDAndAttributes) error {
	if len(sids) == 0 {
		return nil
	}
	entries := make([]windows.EXPLICIT_ACCESS, 0, len(sids))
	for _, sa := range sids {
		entries = append(entries, windows.EXPLICIT_ACCESS{
			AccessPermissions: fileAllAccess,
			AccessMode:        windows.GRANT_ACCESS,
			Trustee: windows.TRUSTEE{
				TrusteeForm:  windows.TRUSTEE_IS_SID,
				TrusteeValue: windows.TrusteeValueFromSID(sa.Sid),
			},
		})
	}
	acl, err := windows.ACLFromEntries(entries, nil)
	if err != nil {
		return err
	}
	type defaultDaclInfo struct {
		defaultDacl *windows.ACL
	}
	info := defaultDaclInfo{defaultDacl: acl}
	return windows.SetTokenInformation(token, uint32(windows.TokenDefaultDacl),
		(*byte)(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)))
}

// logonSidOf 取当前令牌的 logon SID（TokenLogonSid 信息类）。
// 动态 buffer：先问大小再读——固定缓冲在组数多的交互令牌上会
// 因 ERROR_INSUFFICIENT_BUFFER 失败。
func logonSidOf(token windows.Token) (*windows.SID, error) {
	var retLen uint32
	windows.GetTokenInformation(token, windows.TokenLogonSid, nil, 0, &retLen)
	buf := make([]byte, retLen)
	err := windows.GetTokenInformation(token, windows.TokenLogonSid, &buf[0], uint32(len(buf)), &retLen)
	if err != nil {
		return nil, err
	}
	groups := (*windows.Tokengroups)(unsafe.Pointer(&buf[0]))
	if groups.GroupCount == 0 {
		return nil, fmt.Errorf("sandbox: token has no logon sid")
	}
	return groups.Groups[0].Sid, nil
}

// userSidOf 取令牌的用户 SID（TokenUser 信息类）。
// HKCU/用户配置文件的 DACL 普遍只授用户 SID——restricting 集合缺它时，
// WRITE_RESTRICTED 下进程初始化写这些对象被拒（STATUS_DLL_INIT_FAILED）。
func userSidOf(token windows.Token) (*windows.SID, error) {
	var retLen uint32
	windows.GetTokenInformation(token, windows.TokenUser, nil, 0, &retLen)
	buf := make([]byte, retLen)
	err := windows.GetTokenInformation(token, windows.TokenUser, &buf[0], uint32(len(buf)), &retLen)
	if err != nil {
		return nil, err
	}
	user := (*windows.Tokenuser)(unsafe.Pointer(&buf[0]))
	return user.User.Sid, nil
}

// grantDirWrite 保证目录的 DACL 上存在能力 SID 完全访问 ACE（继承到子对象）。
// 幂等：已有该 SID 的完全访问允许 ACE 时跳过——既避免重复调用产生重复 ACE
// （SetEntriesInAcl 不去重），也让目录被删重建后自动重新授权（无进程级缓存）。
func grantDirWrite(dir string, sid *windows.SID) error {
	sd, err := windows.GetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}

	// 幂等检查：已有该能力 SID 的完全访问允许 ACE 则跳过
	if dacl != nil {
		granted, err := aclHasFullGrant(dacl, sid)
		if err != nil {
			return err
		}
		if granted {
			return nil
		}
	}

	entries := []windows.EXPLICIT_ACCESS{{
		AccessPermissions: fileAllAccess,
		AccessMode:        windows.GRANT_ACCESS,
		Inheritance:       subContainersAndObjectsInherit,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeValue: windows.TrusteeValueFromSID(sid),
		},
	}}
	newAcl, err := windows.ACLFromEntries(entries, dacl)
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION, nil, nil, newAcl, nil)
}

// aclHasFullGrant 检查 ACL 是否已有指定 SID 的完全访问允许 ACE
// （直接解析 ACL 内存布局：ACL 头 + ACE 链表，不依赖额外 API）。
func aclHasFullGrant(acl *windows.ACL, sid *windows.SID) (bool, error) {
	if acl == nil {
		return false, nil
	}
	head := (*[8]byte)(unsafe.Pointer(acl))
	aclSize := int(binary.LittleEndian.Uint16(head[2:4]))
	aceCount := int(binary.LittleEndian.Uint16(head[4:6]))
	off := 8
	for i := 0; i < aceCount; i++ {
		if off+8 > aclSize {
			return false, fmt.Errorf("sandbox: acl parse: ace header out of range")
		}
		ace := (*windows.ACE_HEADER)(unsafe.Pointer(uintptr(unsafe.Pointer(acl)) + uintptr(off)))
		if int(ace.AceSize) < 8 || off+int(ace.AceSize) > aclSize {
			return false, fmt.Errorf("sandbox: acl parse: ace size out of range")
		}
		if ace.AceType == windows.ACCESS_ALLOWED_ACE_TYPE {
			aa := (*windows.ACCESS_ALLOWED_ACE)(unsafe.Pointer(ace))
			if aa.Mask&fileAllAccess == fileAllAccess {
				aceSid := (*windows.SID)(unsafe.Pointer(&aa.SidStart))
				if aceSid.String() == sid.String() {
					return true, nil
				}
			}
		}
		off += int(ace.AceSize)
	}
	return false, nil
}

// capabilitySID 派生确定性能力 SID（S-1-4-<a>-<b>，自路径哈希）：
// 同路径（同命名空间）恒得同一 SID，跨进程稳定。
func capabilitySID(ns, path string) (*windows.SID, error) {
	h := sha256.Sum256([]byte(ns + ":" + filepath.Clean(path)))
	a := binary.BigEndian.Uint32(h[0:4]) & 0x7fffffff
	b := binary.BigEndian.Uint32(h[4:8]) & 0x7fffffff
	return windows.StringToSid(fmt.Sprintf("S-1-4-%d-%d", a, b))
}

// probeBackend（windows）：创建空受限令牌探测——成功即可用
// （不实际启动子进程，验证 CreateRestrictedToken 可用即可）。
func probeBackend() sandboxBackend {
	tok, err := createRestrictedToken(nil)
	if err != nil {
		return backendUnavailable
	}
	tok.Close()
	return backendWindowsAcl
}

// planConfined（windows）：受限令牌 + ACL 授权（写白名单）+ per-call deny ACE
// + Job Object 资源限制。
//   - read-only：restricting list 无能力 SID → 除 Everyone 可写对象外全拒
//   - workspace-write：工作区/缓存目录（fsauth.CacheRoots）/追加根（extra）
//     standing ACE + per-call 私有临时目录（TMP/TEMP 指向它），进程结束后清理
//   - deny：per-call 随机 SID 加入 restricting list，对每个 deny 目标追加完全
//     拒绝 ACE（写被拒——WRITE_RESTRICTED 的 pass-2 只覆盖写访问，读不在
//     受限令牌可表达范围（Windows 边界，与 dsh/codex 一致：读侧隔离需另
//     一机制，fs 工具层不受影响）；ACE 继承到子对象，进程结束后撤销）；
//     模式形态不可实例化或可达对象加不上 ACE → 拒绝执行（fail-closed）
//   - 资源限制：Job Object（进程内存 4GiB / job 内存 8GiB / 活动进程 256），
//     spawn 后由 exec_procs assign 子进程（assignJob）；job 句柄随 cleanup 关闭
//   - 返回原样 argv + 令牌句柄 + job 句柄（spawn 后由 exec_procs 使用/关闭）
//
// fsOpen（写全放）与网络规则无法用令牌模型表达，已由 validateProcessPolicy
// 拒绝；fs_allow 通配写授权（writeAllow）同样无法表达——忽略即少授（安全方向）。
func planConfined(spec confineSpec) (launchPlan, error) {
	if err := validateProcessPolicy(spec, "windows"); err != nil {
		return launchPlan{}, err
	}
	if selectBackend() == backendUnavailable {
		return launchPlan{}, sandboxUnavailable(spec.level)
	}
	var extraSids []*windows.SID
	var tmpDir string
	// per-call deny ACE：随机 SID + 目标对象完全拒绝（进程结束后撤销）
	denySid, denyProtected, err := applyDenyACEs(spec.deny)
	if err != nil {
		return launchPlan{}, err
	}
	if denySid != nil {
		extraSids = append(extraSids, denySid)
	}
	cleanup := func() { revokeDenyACEs(denyProtected, denySid) }

	if spec.level >= proto.LevelWrite {
		dirs := make([]string, 0, 4)
		if spec.workdir != "" {
			dirs = append(dirs, spec.workdir)
		}
		dirs = append(dirs, fsauth.CacheRoots()...)
		dirs = append(dirs, publicRoots()...)
		dirs = append(dirs, spec.extra...)
		for _, d := range dirs {
			sid, err := capabilitySID("ws", d)
			if err != nil {
				return launchPlan{}, fmt.Errorf("sandbox: workspace sid: %w", err)
			}
			// 幂等授权：已有 ACE 跳过，目录重建后自动补授
			if err := grantDirWrite(d, sid); err != nil {
				return launchPlan{}, fmt.Errorf("sandbox: grant workspace %s: %w", d, err)
			}
			extraSids = append(extraSids, sid)
		}

		// per-call 私有临时目录：随机路径 + 随机 SID，进程结束后删除
		var err error
		tmpDir, err = os.MkdirTemp("", "aic-sandbox-*")
		if err != nil {
			return launchPlan{}, fmt.Errorf("sandbox: private temp: %w", err)
		}
		tmpSid, err := capabilitySID("tmp", tmpDir)
		if err != nil {
			os.RemoveAll(tmpDir)
			return launchPlan{}, fmt.Errorf("sandbox: temp sid: %w", err)
		}
		if err := grantDirWrite(tmpDir, tmpSid); err != nil {
			os.RemoveAll(tmpDir)
			return launchPlan{}, fmt.Errorf("sandbox: grant temp: %w", err)
		}
		extraSids = append(extraSids, tmpSid)
		cleanup = func() { revokeDenyACEs(denyProtected, denySid); os.RemoveAll(tmpDir) } // ACE 随目录删除消失
	}

	tok, err := createRestrictedToken(extraSids)
	if err != nil {
		cleanup()
		return launchPlan{}, fmt.Errorf("sandbox: restricted token: %w", err)
	}

	// Job Object 资源限制（与令牌/ACL 正交，read-only 与 workspace-write 同限）
	job, err := newJobWithLimits()
	if err != nil {
		tok.Close()
		cleanup()
		return launchPlan{}, fmt.Errorf("sandbox: job object: %w", err)
	}
	cleanup = func() {
		revokeDenyACEs(denyProtected, denySid)
		os.RemoveAll(tmpDir)
		closeJob(uintptr(job))
	}

	env := []string{}
	if tmpDir != "" {
		env = append(env, "TMP="+tmpDir, "TEMP="+tmpDir)
	}
	return launchPlan{argv: spec.argv, token: uintptr(tok), job: uintptr(job), env: env, cleanup: cleanup}, nil
}

// newJobWithLimits 创建 Job Object 并施加资源限制：
//   - JOB_OBJECT_LIMIT_PROCESS_MEMORY：job 内单进程内存上限（4GiB）
//   - JOB_OBJECT_LIMIT_JOB_MEMORY：job 内全部进程合计内存上限（8GiB）
//   - JOB_OBJECT_LIMIT_ACTIVE_PROCESS：job 内活动进程数上限（防 fork 炸弹）
//
// 限制对 job 内所有子孙进程强制（超限即创建失败/分配失败，不会打爆系统）。
func newJobWithLimits() (windows.Handle, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return 0, err
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_PROCESS_MEMORY |
				windows.JOB_OBJECT_LIMIT_JOB_MEMORY |
				windows.JOB_OBJECT_LIMIT_ACTIVE_PROCESS,
			ActiveProcessLimit: resourceLimitJobProcesses,
		},
		ProcessMemoryLimit: uintptr(resourceLimitAS),
		JobMemoryLimit:     uintptr(resourceLimitJobMemory),
	}
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		windows.CloseHandle(job)
		return 0, err
	}
	return job, nil
}

// assignJob 把已启动的进程关联进 Job Object（spawn 后立即调用，限制即生效）。
// 失败必须 fail-closed：调用方负责终止进程并返回错误（不裸跑）。
func assignJob(pid int, job uintptr) error {
	if job == 0 {
		return nil
	}
	proc, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		return err
	}
	defer windows.CloseHandle(proc)
	return windows.AssignProcessToJobObject(windows.Handle(job), proc)
}

// closeJob 关闭 Job Object 句柄（进程结束后调用；job 内进程已全部退出）。
func closeJob(job uintptr) {
	if job != 0 {
		_ = windows.CloseHandle(windows.Handle(job))
	}
}

// applyToken 把受限令牌注入子进程启动属性（windows）。
// 控制台策略保持 SetSysProcAttr 的 CREATE_NO_WINDOW：2026-09-23 真机矩阵
// （TestWindowsConsoleModeMatrix）证实 restricting list 正确携带
// [logon, Everyone]+能力 SID 时，各控制台模式（含 NO_WINDOW/NEW_CONSOLE）
// 在有无控制台父进程下均正常启动——历史 "WRITE_RESTRICTED 下子进程全灭"
// 与 CREATE_NO_WINDOW 无关（实为 CreateRestrictedToken 参数错位致
// restricting 列表为空，见 createRestrictedToken 注）。
func applyToken(cmd *exec.Cmd, token uintptr) error {
	cmd.SysProcAttr.Token = syscall.Token(token)
	return nil
}

// closeToken 关闭令牌句柄（spawn 成功后子进程持有副本，句柄可释放）。
func closeToken(token uintptr) {
	if token != 0 {
		_ = windows.CloseHandle(windows.Handle(token))
	}
}

// ---- per-call deny ACE（fs deny 的 windows 落地）----
//
// 每次 planConfined 生成一个随机 SID（S-1-4-<a>-<b>）加入受限令牌的
// restricting list，并对每个 deny 目标对象追加该 SID 的完全拒绝 ACE：
//   - SetEntriesInAcl 把新 deny ACE 放在 ACL 首部（先于 allow 求值），
//     写访问的 pass-2 检查命中拒绝 → 写被拒（WRITE_DAC/WRITE_OWNER 等
//     写类位一并拒，防沙箱进程自行摘除 ACE）；读不做 pass-2 检查
//     （Windows 边界，见文件头注）；
//   - ACE 继承到子对象（目录整棵子树）；
//   - 该 SID 只存在于本次调用的令牌，宿主机其他进程不受 DACL 变更影响；
//   - 进程结束后以 REVOKE_ACCESS 撤销（子对象继承副本随父对象 ACL 更新消失）。
//
// 可达性判定：加不上 ACE（无 WRITE_DAC）时，若 DACL 未向任何通用 restricted
// SID 授读/写/执行权（沙箱本就不可达）则跳过（语义等价的无操作）；否则
// fail-closed 拒绝执行。

// denyAceMask 是 deny ACE 的拒绝掩码：完全拒绝（读/写/执行/删除/改
// DACL/owner；也是可达性判定时排除元数据位的参考值）。
const denyAceMask = fileAllAccess

// reachMask 是可达性判定的访问位：读/写/执行/删子项（不含 SYNCHRONIZE/
// READ_CONTROL 这类元数据位，避免过度保守）。
const reachMask = 0x120089 | 0x120116 | 0x1200A0 | 0x40

// randomDenySID 生成本次调用的随机 deny SID（两段随机 31 位）。
func randomDenySID() (*windows.SID, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return nil, err
	}
	a := binary.BigEndian.Uint32(b[0:4]) & 0x7fffffff
	c := binary.BigEndian.Uint32(b[4:8]) & 0x7fffffff
	return windows.StringToSid(fmt.Sprintf("S-1-4-%d-%d", a, c))
}

// applyDenyACEs 实例化 deny 模式并对每个目标追加完全拒绝 ACE；返回
// （SID, 已保护目标, error）。形态不可实例化或可达对象加不上 ACE → 错误
// （fail-closed）；对受限令牌本就不可达的对象跳过。
func applyDenyACEs(patterns []string) (*windows.SID, []string, error) {
	if len(patterns) == 0 {
		return nil, nil, nil
	}
	targets, err := denyCoverAll(patterns)
	if err != nil {
		return nil, nil, err
	}
	if len(targets) == 0 {
		return nil, nil, nil
	}
	sid, err := randomDenySID()
	if err != nil {
		return nil, nil, fmt.Errorf("sandbox: deny sid: %w", err)
	}
	restricted := restrictedSIDsForReachability()
	var protected []string
	for _, t := range targets {
		ok, err := addDenyACE(t, sid, restricted)
		if err != nil {
			revokeDenyACEs(protected, sid)
			return nil, nil, err
		}
		if ok {
			protected = append(protected, t)
		}
	}
	return sid, protected, nil
}

// revokeDenyACEs 撤销本调用追加的 deny ACE（best-effort：残留 ACE 只对随机
// SID 生效，无安全影响）。
func revokeDenyACEs(protected []string, sid *windows.SID) {
	if sid == nil {
		return
	}
	for _, t := range protected {
		_ = revokeDenyACE(t, sid)
	}
}

// addDenyACE 在目标对象上追加 deny SID 的完全拒绝 ACE（继承到子对象）。
// ok=false 表示对象对受限令牌本就不可达（DACL 未向通用 restricted SID 授
// 访问权）——跳过是语义等价的无操作；其余失败返回错误（fail-closed）。
func addDenyACE(path string, denySid *windows.SID, restricted []*windows.SID) (bool, error) {
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		// 宿主无 READ_CONTROL：读/改该对象 DACL 均不可行；受限令牌同样拿不到
		// 读授权（READ_CONTROL 普遍对所有用户开放），跳过等价无操作。
		return false, nil
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return false, nil
	}
	entries := []windows.EXPLICIT_ACCESS{{
		AccessPermissions: denyAceMask,
		AccessMode:        windows.DENY_ACCESS,
		Inheritance:       subContainersAndObjectsInherit,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeValue: windows.TrusteeValueFromSID(denySid),
		},
	}}
	if dacl == nil {
		// NULL DACL = 所有人完全访问：追加 Everyone 完全允许条目保持既有
		// 语义（否则只含 deny 的 DACL 会锁死其他进程）。
		everyone, err := windows.CreateWellKnownSid(windows.WinWorldSid)
		if err != nil {
			return false, fmt.Errorf("sandbox: deny %s: %w", path, err)
		}
		entries = append(entries, windows.EXPLICIT_ACCESS{
			AccessPermissions: fileAllAccess,
			AccessMode:        windows.GRANT_ACCESS,
			Trustee: windows.TRUSTEE{
				TrusteeForm:  windows.TRUSTEE_IS_SID,
				TrusteeValue: windows.TrusteeValueFromSID(everyone),
			},
		})
	}
	newAcl, err := windows.ACLFromEntries(entries, dacl)
	if err != nil {
		return false, fmt.Errorf("sandbox: deny acl %s: %w", path, err)
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION, nil, nil, newAcl, nil); err != nil {
		if aclReachable(dacl, restricted) {
			return false, fmt.Errorf("sandbox: deny %s: %w", path, err)
		}
		return false, nil
	}
	return true, nil
}

// removeAcesForSID 返回 DACL 的字节级重建副本：删除 trustee 为该 SID 的
// 全部 ACE（allow/deny 同删），其余 ACE 原样保留。ACL 结构自包含（ACE 内联
// SID），重建 = 拷贝 + 修正头（AclSize/AceCount）。第二个返回值为 false 表示
// DACL 中没有该 SID 的 ACE（无需变更）。
func removeAcesForSID(dacl *windows.ACL, sid *windows.SID) (*windows.ACL, bool, error) {
	if dacl == nil || sid == nil {
		return nil, false, nil
	}
	head := (*[8]byte)(unsafe.Pointer(dacl))
	aclSize := int(binary.LittleEndian.Uint16(head[2:4]))
	aceCount := int(binary.LittleEndian.Uint16(head[4:6]))
	if aclSize < 8 {
		return nil, false, fmt.Errorf("sandbox: acl parse: implausible size %d", aclSize)
	}
	buf := make([]byte, 8, aclSize)
	copy(buf, head[:])
	want := sid.String()
	kept := 0
	removed := false
	off := 8
	for i := 0; i < aceCount; i++ {
		if off+8 > aclSize {
			return nil, false, fmt.Errorf("sandbox: acl parse: ace header out of range")
		}
		ace := (*windows.ACE_HEADER)(unsafe.Pointer(uintptr(unsafe.Pointer(dacl)) + uintptr(off)))
		size := int(ace.AceSize)
		if size < 8 || off+size > aclSize {
			return nil, false, fmt.Errorf("sandbox: acl parse: ace size out of range")
		}
		match := false
		switch ace.AceType {
		case windows.ACCESS_ALLOWED_ACE_TYPE, windows.ACCESS_DENIED_ACE_TYPE:
			aa := (*windows.ACCESS_ALLOWED_ACE)(unsafe.Pointer(ace))
			aceSid := (*windows.SID)(unsafe.Pointer(&aa.SidStart))
			match = aceSid.String() == want
		}
		if match {
			removed = true
		} else {
			buf = append(buf, (*[(1 << 16) - 1]byte)(unsafe.Pointer(ace))[:size:size]...)
			kept++
		}
		off += size
	}
	if !removed {
		return nil, false, nil
	}
	buf[2] = byte(len(buf))
	buf[3] = byte(len(buf) >> 8)
	buf[4] = byte(kept)
	buf[5] = byte(kept >> 8)
	return (*windows.ACL)(unsafe.Pointer(&buf[0])), true, nil
}

// revokeDenyACE 撤销本调用追加的 deny ACE：取对象当前 DACL，字节级重建副本
// （删除该 SID 的全部 ACE）后应用。不走 SetEntriesInAcl 的 REVOKE_ACCESS
// 合并——win11 26200 实测该路径返回 ERROR_SUCCESS 但 deny 类 ACE 残留
// （带/不带继承位均如此；同样形态在 codex/dsh 环境可用，本机行为未再归因），
// 字节级重建与 ACL 编辑器删除 ACE 同语义。返回错误供诊断/调用方处理
// （revokeDenyACEs 仍为 best-effort 语义）。
func revokeDenyACE(path string, denySid *windows.SID) error {
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	newAcl, changed, err := removeAcesForSID(dacl, denySid)
	if err != nil || !changed {
		return err
	}
	return windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION, nil, nil, newAcl, nil)
}

// restrictedSIDsForReachability 汇总 restricting list 中的通用 SID
// （Everyone/INTERACTIVE/Authenticated Users/BUILTIN Users/用户/logon）——
// 「加不上 deny ACE 时对象对沙箱是否可达」的判定集合。
func restrictedSIDsForReachability() []*windows.SID {
	out := make([]*windows.SID, 0, 6)
	for _, wks := range []windows.WELL_KNOWN_SID_TYPE{
		windows.WinWorldSid,
		windows.WinInteractiveSid,
		windows.WinAuthenticatedUserSid,
		windows.WinBuiltinUsersSid,
	} {
		if s, err := windows.CreateWellKnownSid(wks); err == nil {
			out = append(out, s)
		}
	}
	tok := windows.GetCurrentProcessToken()
	if s, err := userSidOf(tok); err == nil {
		out = append(out, s)
	}
	if s, err := logonSidOf(tok); err == nil {
		out = append(out, s)
	}
	return out
}

// aclReachable 判定 DACL 是否向给定 SID 集授予读/写/执行/删子项权。
// nil DACL = 所有人完全访问 → 恒可达；解析异常按可达处理（保守）。
func aclReachable(dacl *windows.ACL, sids []*windows.SID) bool {
	if dacl == nil {
		return true
	}
	head := (*[8]byte)(unsafe.Pointer(dacl))
	aclSize := int(binary.LittleEndian.Uint16(head[2:4]))
	aceCount := int(binary.LittleEndian.Uint16(head[4:6]))
	off := 8
	for i := 0; i < aceCount; i++ {
		if off+8 > aclSize {
			return true
		}
		ace := (*windows.ACE_HEADER)(unsafe.Pointer(uintptr(unsafe.Pointer(dacl)) + uintptr(off)))
		if int(ace.AceSize) < 8 || off+int(ace.AceSize) > aclSize {
			return true
		}
		if ace.AceType == windows.ACCESS_ALLOWED_ACE_TYPE {
			aa := (*windows.ACCESS_ALLOWED_ACE)(unsafe.Pointer(ace))
			if aa.Mask&reachMask != 0 {
				aceSid := (*windows.SID)(unsafe.Pointer(&aa.SidStart))
				for _, s := range sids {
					if s != nil && aceSid.String() == s.String() {
						return true
					}
				}
			}
		}
		off += int(ace.AceSize)
	}
	return false
}
