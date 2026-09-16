//go:build windows

// Windows 沙箱运行时集成测试（需真实 Windows 环境；mbp 交叉编译不执行）。
// 验证受限令牌 + ACL 写授权的实际行为：
//   - read-only：写任意路径被拒
//   - workspace-write：写工作区成功、写外部路径被拒、TMP/TEMP 指向私有目录
//   - cleanup：私有临时目录在进程结束后被删除
package exec_procs

import (
	"bytes"
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
// 三段式 Start → assignJob（Job Object 资源限制生效）→ Wait，
// 与 exec_procs.Start 同生命周期（spawn 后关闭令牌、进程结束后 cleanup）。
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
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Start(); err != nil {
		return "", -1
	}
	if plan.job != 0 {
		if err := assignJob(cmd.Process.Pid, plan.job); err != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			return "", -1
		}
	}
	err := cmd.Wait()
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
	return buf.String(), exit
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
	plan, err := planConfined(confineSpec{fsOpen: true, netOpen: true, level: proto.LevelRead, workdir: ws, argv: writeCmd(target)})
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
func TestWindowsSandboxRejectsUnsupportedReadScope(t *testing.T) {
	_, err := planConfined(confineSpec{level: 9, workdir: t.TempDir(), argv: []string{"cmd", "/c", "echo ok"}, netOpen: true})
	if err == nil || !strings.Contains(err.Error(), "cannot enforce") {
		t.Fatalf("unsupported closed policy must reject: %v", err)
	}
}

// TMP/TEMP 注入：受限进程看到的是私有临时目录。
func TestWindowsSandboxTempEnv(t *testing.T) {
	ws := t.TempDir()
	plan, err := planConfined(confineSpec{fsOpen: true, netOpen: true, level: proto.LevelWrite, workdir: ws, argv: []string{"cmd", "/c", "echo TMP=[%TMP%]"}})
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
	plan, err := planConfined(confineSpec{fsOpen: true, netOpen: true, level: proto.LevelWrite, workdir: ws, argv: []string{"cmd", "/c", "echo %TMP%"}})
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

// Job Object 资源限制：创建成功、限制 flags 正确（进程内存 4GiB / job 内存
// 8GiB / 活动进程 256），受限令牌子进程 assign 后正常运行（集成路径）。
func TestWindowsJobLimits(t *testing.T) {
	job, err := newJobWithLimits()
	if err != nil {
		t.Fatalf("newJobWithLimits: %v", err)
	}
	defer windows.CloseHandle(job)

	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	var retLen uint32
	if err := windows.QueryInformationJobObject(job, int32(windows.JobObjectExtendedLimitInformation),
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)), &retLen); err != nil {
		t.Fatalf("QueryInformationJobObject: %v", err)
	}
	flags := info.BasicLimitInformation.LimitFlags
	for _, f := range []uint32{
		windows.JOB_OBJECT_LIMIT_PROCESS_MEMORY,
		windows.JOB_OBJECT_LIMIT_JOB_MEMORY,
		windows.JOB_OBJECT_LIMIT_ACTIVE_PROCESS,
	} {
		if flags&f == 0 {
			t.Fatalf("job flags 0x%x missing 0x%x", flags, f)
		}
	}
	if info.ProcessMemoryLimit != uintptr(resourceLimitAS) {
		t.Fatalf("ProcessMemoryLimit = %d, want %d", info.ProcessMemoryLimit, resourceLimitAS)
	}
	if info.JobMemoryLimit != uintptr(resourceLimitJobMemory) {
		t.Fatalf("JobMemoryLimit = %d, want %d", info.JobMemoryLimit, resourceLimitJobMemory)
	}
	if info.BasicLimitInformation.ActiveProcessLimit != resourceLimitJobProcesses {
		t.Fatalf("ActiveProcessLimit = %d, want %d", info.BasicLimitInformation.ActiveProcessLimit, resourceLimitJobProcesses)
	}

	// planConfined 集成：job 句柄随 plan 返回，子进程 assign 后正常执行
	ws := t.TempDir()
	plan, err := planConfined(confineSpec{fsOpen: true, netOpen: true, level: proto.LevelWrite, workdir: ws, argv: []string{"cmd", "/c", "echo ok"}})
	if err != nil {
		t.Fatalf("planConfined: %v", err)
	}
	if plan.job == 0 {
		t.Fatal("planConfined should attach a job object")
	}
	out, exit := runWithPlan(t, plan, plan.argv, ws)
	if exit != 0 || !strings.Contains(out, "ok") {
		t.Fatalf("job-attached run failed: exit=%d out=%q", exit, out)
	}
}
