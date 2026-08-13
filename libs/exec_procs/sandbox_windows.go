//go:build windows

// Windows 沙箱后端（路径 A）：受限令牌（CreateRestrictedToken）+ ACL 写授权，
// host 进程内创建令牌后经 SysProcAttr.Token 注入子进程，无独立 runner。
//
// 机制（与 dsh sandbox-windows-acl 同模型，Go 原生实现）：
//   - 受限令牌的 restricting list = logon SID + Everyone（两者必须保留——
//     进程初始化依赖 Everyone）+ 能力 SID（工作区/私有临时目录）
//   - 能力 SID 是确定性派生（路径哈希）或随机（私有临时目录）的自定义 SID，
//     对应目录的 DACL 上授予完全访问 ACE：
//     - 工作区：standing（进程级缓存，跨调用复用；host 退出残留无害——
//       下次精确跳过，同 dsh 语义）
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
	"sync"
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
	// 部分转发（实测 Win11 26200：CreateRestrictedToken 在 advapi32、
	// SetEntriesInAcl 仅 api-ms-win-security-provider）。按序查找全部候选。
	securityProviderDll = windows.NewLazySystemDLL("api-ms-win-security-provider-l1-1-0.dll")
	securityBaseDll     = windows.NewLazySystemDLL("api-ms-win-security-base-l1-2-0.dll")

	// 注：SetEntriesInAcl 的导出名带 A/W 后缀（SetEntriesInAclW），
	// advapi32 无无后缀导出——其余 API 均无后缀。
	procCreateRestrictedToken     = findProcAny("CreateRestrictedToken", advapi32, kernel32, securityBaseDll)
	procSetEntriesInAcl           = findProcAny("SetEntriesInAclW", advapi32, kernel32, securityProviderDll, securityBaseDll)
	procGetSecurityDescriptorDacl = findProcAny("GetSecurityDescriptorDacl", advapi32, kernel32, securityBaseDll)
	procSetTokenInformation       = findProcAny("SetTokenInformation", advapi32, kernel32, securityBaseDll)
	procLocalFree                 = findProcAny("LocalFree", advapi32, kernel32)
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

// trustee/explictAccess 是 C EXPLICIT_ACCESS 的 Go 布局
// （TRUSTEE: 4×uint32 + LPWSTR；64 位下对齐 8 字节）。
type trustee struct {
	multipleTrustee          *trustee
	multipleTrusteeOperation uint32
	trusteeForm              uint32
	trusteeType              uint32
	trusteeName              *uint16
}

type explicitAccess struct {
	accessPermissions uint32
	accessMode        uint32
	inheritance       uint32
	trustee           trustee
}

const (
	trusteeIsSid                   = 1 // TRUSTEE_IS_SID：ptstrName 是 PSID
	subContainersAndObjectsInherit = 3 // 子目录 + 文件继承 ACE
	fileAllAccess                  = 0x001F01FF
	genericAll                     = 0x10000000

	// CreateRestrictedToken Flags（对齐 codex/dsh：写受限 + LUA + 禁用特权）
	disableMaxPrivilege = 0x1
	luaToken            = 0x04
	writeRestricted     = 0x08

	// TokenDefaultDacl 信息类（TOKEN_INFORMATION_CLASS 枚举值）
	tokenDefaultDacl = 6
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
	restrictions := []windows.SIDAndAttributes{
		{Sid: logonSid},
		{Sid: everyone},
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
		uintptr(disableMaxPrivilege|luaToken|writeRestricted),
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
// GENERIC_ALL）：沙箱进程新建对象（pipe/IPC）时无需逐对象授权。
func setDefaultDacl(token windows.Token, sids []windows.SIDAndAttributes) error {
	if len(sids) == 0 {
		return nil
	}
	if procSetEntriesInAcl == nil {
		return procMissing("SetEntriesInAcl")
	}
	if procSetTokenInformation == nil {
		return procMissing("SetTokenInformation")
	}
	entries := make([]explicitAccess, 0, len(sids))
	for _, sa := range sids {
		entries = append(entries, explicitAccess{
			accessPermissions: genericAll,
			accessMode:        windows.GRANT_ACCESS,
			trustee: trustee{
				trusteeForm: trusteeIsSid,
				trusteeName: (*uint16)(unsafe.Pointer(sa.Sid)),
			},
		})
	}
	var newAcl *windows.ACL
	r1, _, e1 := procSetEntriesInAcl.Call(
		uintptr(len(entries)),
		uintptr(unsafe.Pointer(&entries[0])),
		0,
		uintptr(unsafe.Pointer(&newAcl)),
	)
	if r1 == 0 {
		return e1
	}
	defer procLocalFree.Call(uintptr(unsafe.Pointer(newAcl)))
	type defaultDaclInfo struct {
		defaultDacl *windows.ACL
	}
	info := defaultDaclInfo{defaultDacl: newAcl}
	r1, _, e1 = procSetTokenInformation.Call(
		uintptr(token),
		uintptr(tokenDefaultDacl),
		uintptr(unsafe.Pointer(&info)),
		uintptr(unsafe.Sizeof(info)),
	)
	if r1 == 0 {
		return e1
	}
	return nil
}

// logonSidOf 取当前令牌的 logon SID（TokenLogonSid 信息类）。
func logonSidOf(token windows.Token) (*windows.SID, error) {
	var buf [256]byte // TOKEN_GROUPS 头 + 1 组
	var retLen uint32
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

// grantDirWrite 给目录的 DACL 追加一条能力 SID 完全访问 ACE（继承到子对象）。
func grantDirWrite(dir string, sid *windows.SID) error {
	if procGetSecurityDescriptorDacl == nil || procSetEntriesInAcl == nil || procLocalFree == nil {
		return procMissing("GetSecurityDescriptorDacl/SetEntriesInAcl/LocalFree")
	}
	sd, err := windows.GetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	var daclPresent int32
	var dacl *windows.ACL
	var daclDefaulted int32
	r1, _, e1 := procGetSecurityDescriptorDacl.Call(
		uintptr(unsafe.Pointer(sd)),
		uintptr(unsafe.Pointer(&daclPresent)),
		uintptr(unsafe.Pointer(&dacl)),
		uintptr(unsafe.Pointer(&daclDefaulted)),
	)
	if r1 == 0 {
		return e1
	}
	if daclPresent == 0 {
		dacl = nil // 无 DACL 对象：以空 ACL 追加（保持现有语义不变）
	}

	ea := explicitAccess{
		accessPermissions: fileAllAccess,
		accessMode:        windows.GRANT_ACCESS,
		inheritance:       subContainersAndObjectsInherit,
		trustee: trustee{
			trusteeForm: trusteeIsSid,
			trusteeName: (*uint16)(unsafe.Pointer(sid)),
		},
	}
	var newAcl *windows.ACL
	r1, _, e1 = procSetEntriesInAcl.Call(
		1,
		uintptr(unsafe.Pointer(&ea)),
		uintptr(unsafe.Pointer(dacl)),
		uintptr(unsafe.Pointer(&newAcl)),
	)
	if r1 == 0 {
		return e1
	}
	defer procLocalFree.Call(uintptr(unsafe.Pointer(newAcl)))
	return windows.SetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION, nil, nil, newAcl, nil)
}

// capabilitySID 派生确定性能力 SID（S-1-4-<a>-<b>，自路径哈希）：
// 同路径（同命名空间）恒得同一 SID，跨进程稳定。
func capabilitySID(ns, path string) (*windows.SID, error) {
	h := sha256.Sum256([]byte(ns + ":" + filepath.Clean(path)))
	a := binary.BigEndian.Uint32(h[0:4]) & 0x7fffffff
	b := binary.BigEndian.Uint32(h[4:8]) & 0x7fffffff
	return windows.StringToSid(fmt.Sprintf("S-1-4-%d-%d", a, b))
}

// wsGranted 是工作区 standing ACE 的进程级缓存（已材料化即跳过）。
var wsGranted sync.Map // path → struct{}

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
			if _, ok := wsGranted.Load(d); !ok {
				if err := grantDirWrite(d, sid); err != nil {
					return launchPlan{}, fmt.Errorf("sandbox: grant workspace %s: %w", d, err)
				}
				wsGranted.Store(d, struct{}{})
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
