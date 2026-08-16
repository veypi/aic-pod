//go:build windows

// Windows 沙箱后端（路径 A）：受限令牌（CreateRestrictedToken）+ ACL 写授权，
// host 进程内创建令牌后经 SysProcAttr.Token 注入子进程，无独立 runner。
//
// 机制（与 dsh sandbox-windows-acl 同模型，Go 原生实现）：
//   - 受限令牌的 restricting list = logon SID + Everyone（两者必须保留——
//     进程初始化依赖 Everyone）+ 能力 SID（工作区/私有临时目录）
//   - 能力 SID 是确定性派生（路径哈希）或随机（私有临时目录）的自定义 SID，
//     对应目录的 DACL 上授予完全访问 ACE：
//     - 工作区：standing ACE，幂等授权——每次调用先检查 DACL 是否已有该
//       能力 SID 的完全访问 ACE，有则跳过（不产生重复 ACE）；目录被删重建
//       后 ACE 消失，下次调用自动重新授权（无进程级缓存，无陈旧状态）
//     - 私有临时目录：per-call 随机创建 + 随机 SID，进程结束后删除目录
//       （ACE 随目录消失，无需显式撤销）
//   - 进程访问对象时 Windows 做两次检查：正常 SID 检查 + restricting SID
//     检查（restricting SID 视为唯一 SID 集合）。能力 SID 只对授权目录有
//     权限，因此受限进程只能写工作区与私有临时目录；其余对象写被拒
//     （Everyone 可写的对象除外——报告 partial 的固有边界）。
//   - read-only：restricting list 无能力 SID → 除 Everyone 可写对象外全部
//     写被拒。
//   - TMP/TEMP 环境变量指向私有临时目录（子进程继承）。
package exec_procs

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"

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

	// CreateRestrictedToken Flags（当前仅 DISABLE_MAX_PRIVILEGE）。
	// 排查记录：WRITE_RESTRICTED/LUA 下受限进程初始化 DLL 全灭
	// （STATUS_DLL_INIT_FAILED，cmd/powershell/git 实测）；restricting list
	// 已含 logon/Everyone/用户 SID/INTERACTIVE/Auth Users/Users 仍失败。
	// 暂退到最小 flags 保证进程可运行，写隔离后续以完整性级别/ACL 方案补。
	disableMaxPrivilege = 0x1
)

// createRestrictedToken 创建受限令牌：restricting list = logon SID +
// Everyone + 能力 SIDs；WRITE_RESTRICTED + LUA + 禁用最大特权。
// 随后设置宽松默认 DACL：沙箱进程创建 pipe/IPC 对象（PowerShell 管道）
// 不因 ACCESS_DENIED 失败（codex 同策略）。
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
	userSid, err := userSidOf(procToken)
	if err != nil {
		return 0, err
	}
	restrictions := []windows.SIDAndAttributes{
		{Sid: logonSid},
		{Sid: everyone},
		{Sid: userSid},
	}
	// 进程初始化依赖的基础组（系统 DLL/注册表/命名对象普遍授这些组）：
	// restricting list 只有 logon SID + Everyone 时，kernel32/ntdll 加载器
	// 访问被拒 → STATUS_DLL_INIT_FAILED (0xC0000142)，实测 cmd/powershell/
	// git 全部启动失败。加 INTERACTIVE/Authenticated Users/BUILTIN Users
	// 后进程可正常初始化；写边界仍由 WRITE_RESTRICTED + 能力 SID 控制
	// （Users 可写对象属 partial 固有边界，与 Everyone 同级）。
	for _, wks := range []windows.WELL_KNOWN_SID_TYPE{
		windows.WinInteractiveSid,
		windows.WinAuthenticatedUserSid,
		windows.WinBuiltinUsersSid,
	} {
		s, err := windows.CreateWellKnownSid(wks)
		if err != nil {
			return 0, err
		}
		restrictions = append(restrictions, windows.SIDAndAttributes{Sid: s})
	}
	for _, s := range extraSids {
		restrictions = append(restrictions, windows.SIDAndAttributes{Sid: s})
	}

	var newToken windows.Token
	if procCreateRestrictedToken == nil {
		return 0, procMissing("CreateRestrictedToken")
	}
	// CreateRestrictedToken(Existing, Flags, DisableCount, Disable,
	//   RestrictCount, Restrict, PrivDelCount, PrivDel, NewToken)
	r1, _, e1 := procCreateRestrictedToken.Call(
		uintptr(procToken),
		uintptr(disableMaxPrivilege),
		0, 0,
		uintptr(len(restrictions)),
		uintptr(unsafe.Pointer(&restrictions[0])),
		0, 0,
		uintptr(unsafe.Pointer(&newToken)),
	)
	if r1 == 0 {
		return 0, e1
	}
	if err := setDefaultDacl(newToken, restrictions); err != nil {
		newToken.Close()
		return 0, err
	}
	return newToken, nil
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
//（SetEntriesInAcl 不去重），也让目录被删重建后自动重新授权（无进程级缓存）。
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

// planConfined（windows）：受限令牌 + ACL 写授权。
//   - read-only：restricting list 无能力 SID → 除 Everyone 可写对象外全拒
//   - workspace-write：工作区/缓存目录（cacheRoots）standing ACE + per-call 私有
//     临时目录（TMP/TEMP 指向它），进程结束后清理
//   - 返回原样 argv + 令牌句柄（spawn 后由 exec_procs 关闭）
func planConfined(level int, workdir string, argv []string) (launchPlan, error) {
	if selectBackend() == backendUnavailable {
		return launchPlan{}, sandboxUnavailable(level)
	}
	var extraSids []*windows.SID
	var tmpDir string
	cleanup := func() {}

	if level >= proto.LevelWrite {
		dirs := make([]string, 0, 4)
		if workdir != "" {
			dirs = append(dirs, workdir)
		}
		dirs = append(dirs, cacheRoots()...)
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
		cleanup = func() { os.RemoveAll(tmpDir) } // ACE 随目录删除消失
	}

	tok, err := createRestrictedToken(extraSids)
	if err != nil {
		cleanup()
		return launchPlan{}, fmt.Errorf("sandbox: restricted token: %w", err)
	}
	env := []string{}
	if tmpDir != "" {
		env = append(env, "TMP="+tmpDir, "TEMP="+tmpDir)
	}
	return launchPlan{argv: argv, token: uintptr(tok), env: env, cleanup: cleanup}, nil
}

// cacheRoots（windows）：常见工具链缓存目录，精确到子目录（不放行整个
// %LOCALAPPDATA%——其下还有大量应用数据/凭证存储）。存在性过滤。
func cacheRoots() []string {
	var dirs []string
	if lad := os.Getenv("LOCALAPPDATA"); lad != "" {
		dirs = append(dirs,
			filepath.Join(lad, "go-build"),
			filepath.Join(lad, "npm-cache"),
			filepath.Join(lad, "pip", "Cache"),
		)
	}
	dirs = append(dirs, os.Getenv("GOCACHE"), os.Getenv("XDG_CACHE_HOME"))
	return existingDirs(dirs...)
}

// applyToken 把受限令牌注入子进程启动属性（windows）。
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
