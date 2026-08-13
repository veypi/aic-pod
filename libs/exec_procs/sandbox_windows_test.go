//go:build windows

// Windows 沙箱运行时集成测试（需真实 Windows 环境；mbp 交叉编译不执行）。
// 验证受限令牌 + ACL 写授权的实际行为：
//   - read-only：写任意路径被拒
//   - workspace-write：写工作区成功、写外部路径被拒、TMP/TEMP 指向私有目录
//   - cleanup：私有临时目录在进程结束后被删除
package exec_procs

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/veypi/aic-pod/libs/proto"
)

// runWithPlan 以 launchPlan 启动命令并等待，返回输出与退出码。
// spawn 后关闭令牌句柄、执行 cleanup（与 exec_procs.Start 同生命周期）。
func runWithPlan(t *testing.T, plan launchPlan, argv []string, workdir string) (string, int) {
	t.Helper()
	if plan.token == 0 {
		t.Fatal("expected a restricted token")
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = workdir
	if plan.env != nil {
		cmd.Env = append(os.Environ(), plan.env...)
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Token: syscall.Token(plan.token)}
	out, err := cmd.CombinedOutput()
	exit := 0
	if ee, ok := err.(*exec.ExitError); ok {
		exit = ee.ExitCode()
	} else if err != nil {
		exit = -1
	}
	closeToken(plan.token)
	if plan.cleanup != nil {
		plan.cleanup()
	}
	return string(out), exit
}

// writeCmd 构造 cmd /c 写文件命令（> 重定向）。
func writeCmd(path string) []string {
	return []string{"cmd", "/c", "echo x > " + path}
}

func TestWindowsSandboxProbe(t *testing.T) {
	if backend := probeBackend(); backend != backendWindowsAcl {
		tok, err := createRestrictedToken(nil)
		if err != nil {
			t.Fatalf("probeBackend=%v; createRestrictedToken error: %v", backend, err)
		}
		tok.Close()
		t.Fatalf("probeBackend should report windows-acl backend, got %v", backend)
	}
}

// 诊断：定位哪个 Flags 组合在普通用户下被拒；检查当前令牌是否本身受限。
func TestWindowsCreateRestrictedTokenFlagMatrix(t *testing.T) {
	procToken, err := windows.OpenCurrentProcessToken()
	if err != nil {
		t.Fatalf("OpenCurrentProcessToken: %v", err)
	}
	defer procToken.Close()

	// 当前令牌受限状态（OpenSSH 会话可能已是 restricted token）
	var buf [4096]byte
	var retLen uint32
	if err := windows.GetTokenInformation(procToken, windows.TokenRestrictedSids, &buf[0], uint32(len(buf)), &retLen); err == nil {
		groups := (*windows.Tokengroups)(unsafe.Pointer(&buf[0]))
		t.Logf("TokenRestrictedSids count=%d", groups.GroupCount)
	} else {
		t.Logf("TokenRestrictedSids query failed: %v", err)
	}
	var elev int32
	if err := windows.GetTokenInformation(procToken, windows.TokenElevation, (*byte)(unsafe.Pointer(&elev)), 4, &retLen); err == nil {
		t.Logf("TokenElevation=%d", elev)
	}

	everyone, err := windows.CreateWellKnownSid(windows.WinWorldSid)
	if err != nil {
		t.Fatalf("CreateWellKnownSid: %v", err)
	}

	// 空限制列表
	var tok0 windows.Token
	r1, _, e1 := procCreateRestrictedToken.Call(
		uintptr(procToken), 0x1, 0, 0, 0, 0, 0, 0, uintptr(unsafe.Pointer(&tok0)))
	if r1 == 0 {
		t.Logf("empty restrictions: FAILED %v", e1)
	} else {
		t.Logf("empty restrictions: OK")
		tok0.Close()
	}

	// TOKEN_ALL_ACCESS 打开后重试
	var allTok windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_ALL_ACCESS, &allTok); err == nil {
		defer allTok.Close()
		restrictions := []windows.SIDAndAttributes{{Sid: everyone}}
		var tokA windows.Token
		r1, _, e1 := procCreateRestrictedToken.Call(
			uintptr(allTok), 0x1,
			0, 0,
			uintptr(len(restrictions)),
			uintptr(unsafe.Pointer(&restrictions[0])),
			0, 0,
			uintptr(unsafe.Pointer(&tokA)),
		)
		if r1 == 0 {
			t.Logf("TOKEN_ALL_ACCESS handle: FAILED %v", e1)
		} else {
			t.Logf("TOKEN_ALL_ACCESS handle: OK")
			tokA.Close()
		}
	} else {
		t.Logf("OpenProcessToken ALL_ACCESS failed: %v", err)
	}

	for _, flags := range []uintptr{0x1, 0x1 | 0x8, 0x1 | 0x4, 0x1 | 0x4 | 0x8} {
		restrictions := []windows.SIDAndAttributes{{Sid: everyone}}
		var tok windows.Token
		r1, _, e1 := procCreateRestrictedToken.Call(
			uintptr(procToken), flags,
			0, 0,
			uintptr(len(restrictions)),
			uintptr(unsafe.Pointer(&restrictions[0])),
			0, 0,
			uintptr(unsafe.Pointer(&tok)),
		)
		if r1 == 0 {
			t.Logf("flags=0x%x: FAILED %v", flags, e1)
		} else {
			t.Logf("flags=0x%x: OK", flags)
			tok.Close()
		}
	}
}

// read-only：无能力 SID，写工作区也被拒（Everything 可写对象除外）。
func TestWindowsSandboxReadOnlyDeniesWrite(t *testing.T) {
	ws := t.TempDir()
	target := filepath.Join(ws, "ro.txt")
	plan, err := planConfined(proto.LevelRead, ws, writeCmd(target))
	if err != nil {
		t.Fatalf("planConfined: %v", err)
	}
	out, exit := runWithPlan(t, plan, plan.argv, ws)
	if exit == 0 {
		t.Fatalf("read-only write should fail: %s", out)
	}
	if _, err := os.Stat(target); err == nil {
		t.Fatal("read-only target file must not exist")
	}
}

// workspace-write：写工作区成功；写外部路径被拒；TMP/TEMP 指向私有目录。
func TestWindowsSandboxWorkspaceWrite(t *testing.T) {
	ws := t.TempDir()
	outside := filepath.Join(os.TempDir(), "aic-sb-outside-"+strings.Repeat("x", 8)+".txt")
	os.Remove(outside)
	defer os.Remove(outside)

	// 写工作区 → 成功
	ok := filepath.Join(ws, "ok.txt")
	plan, err := planConfined(proto.LevelWrite, ws, writeCmd(ok))
	if err != nil {
		t.Fatalf("planConfined: %v", err)
	}
	if _, exit := runWithPlan(t, plan, plan.argv, ws); exit != 0 {
		t.Fatalf("workspace write should succeed")
	}
	if _, err := os.Stat(ok); err != nil {
		t.Fatalf("workspace file missing: %v", err)
	}

	// 写外部路径 → 拒绝
	plan2, err := planConfined(proto.LevelWrite, ws, writeCmd(outside))
	if err != nil {
		t.Fatalf("planConfined: %v", err)
	}
	if _, exit := runWithPlan(t, plan2, plan2.argv, ws); exit == 0 {
		t.Fatal("outside write should fail")
	}
	if _, err := os.Stat(outside); err == nil {
		t.Fatal("outside file must not exist")
	}
}

// TMP/TEMP 注入：受限进程看到的是私有临时目录。
func TestWindowsSandboxTempEnv(t *testing.T) {
	ws := t.TempDir()
	plan, err := planConfined(proto.LevelWrite, ws, []string{"cmd", "/c", "echo TMP=[%TMP%]"})
	if err != nil {
		t.Fatalf("planConfined: %v", err)
	}
	out, exit := runWithPlan(t, plan, plan.argv, ws)
	if exit != 0 || !strings.Contains(out, "aic-sandbox") {
		t.Fatalf("TMP should point at private aic-sandbox dir: exit=%d out=%q", exit, out)
	}
}

// cleanup：进程结束后私有临时目录被删除。
func TestWindowsSandboxCleanupRemovesTemp(t *testing.T) {
	ws := t.TempDir()
	plan, err := planConfined(proto.LevelWrite, ws, []string{"cmd", "/c", "echo %TMP%"})
	if err != nil {
		t.Fatalf("planConfined: %v", err)
	}
	tmpDir := ""
	for _, kv := range plan.env {
		if strings.HasPrefix(kv, "TMP=") {
			tmpDir = strings.TrimPrefix(kv, "TMP=")
		}
	}
	if tmpDir == "" {
		t.Fatal("plan.env missing TMP")
	}
	if _, err := os.Stat(tmpDir); err != nil {
		t.Fatalf("private temp should exist before run: %v", err)
	}
	runWithPlan(t, plan, plan.argv, ws)
	if _, err := os.Stat(tmpDir); err == nil {
		t.Fatal("private temp should be removed after run")
	}
}
