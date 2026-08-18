package browser

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/veypi/aic-pod/libs/exec_procs"
	"github.com/veypi/aic-pod/libs/vcore"
)

// stubCLI 写一个 agent-browser stub：把调用参数追加到 log 文件，
// 按子命令输出固定内容；download/screenshot/state 需要真实落文件。
func stubCLI(t *testing.T) (execPath, logPath string) {
	t.Helper()
	dir := t.TempDir()
	logPath = filepath.Join(dir, "cli.log")
	script := fmt.Sprintf(`#!/bin/sh
echo "$@" >> %q
# 最后一个参数为落盘路径的子命令
for a in "$@"; do last="$a"; done
case " $* " in
  *" download "*) echo "file-bytes" > "$last" ;;
  *" screenshot "*) echo "jpeg-bytes" > "$last" ;;
  *" pdf "*) echo "pdf-bytes" > "$last" ;;
  *" state save "*) echo "state-json" > "$last" ;;
  *" open "*) printf '✓ Example Domain\n  https://example.com/\n' ;;
  *" snapshot "*) printf -- '- heading "T" [level=1, ref=e1]\n- link "L" [ref=e2]\n' ;;
  *" session list "*) exit 1 ;; # 会话不存活 → open 触发 state 自动加载
  *) echo "ok-$3" ;;
esac
`, logPath)
	execPath = filepath.Join(dir, "agent-browser")
	if err := os.WriteFile(execPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return execPath, logPath
}

func readLog(t *testing.T, logPath string) string {
	t.Helper()
	data, _ := os.ReadFile(logPath)
	return string(data)
}

func TestBrowserOpenAndStateFlow(t *testing.T) {
	execPath, logPath := stubCLI(t)
	statePath := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(statePath, []byte("old-state"), 0o600); err != nil {
		t.Fatal(err)
	}
	vfs := vcore.NewMemVFS()
	env := &vcore.Env{VFS: vfs, Workdir: "/sessions/s1", Vars: map[string]string{"$SESSION": "/sessions/s1"}}
	b := New(Config{ExecPath: execPath, Namespace: "u1", Session: "s1", StatePath: statePath})
	defer b.Close()

	res, err := b.Handle(context.Background(), env, "msg-1", []string{"open", "https://example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Content, "✓ Example Domain") {
		t.Errorf("content = %q", res.Content)
	}
	log := readLog(t, logPath)
	if !strings.Contains(log, "--state "+statePath) {
		t.Errorf("first open should autoload state, log = %q", log)
	}
	if !strings.Contains(log, "--namespace u1 --session s1") {
		t.Errorf("isolation flags missing, log = %q", log)
	}

	// state 自动保存：Close 时冲刷（dirty 由 open 标记）
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(statePath)
	if strings.TrimSpace(string(data)) != "state-json" {
		t.Errorf("state not auto-saved on close, = %q", data)
	}
}

func TestBrowserUploadDownloadScreenshot(t *testing.T) {
	execPath, logPath := stubCLI(t)
	vfs := vcore.NewMemVFS()
	vfs.SetFile("/sessions/s1/report.pdf", []byte("pdf-bytes"), time.Now())
	env := &vcore.Env{VFS: vfs, Workdir: "/sessions/s1", Vars: map[string]string{"$SESSION": "/sessions/s1"}}
	b := New(Config{ExecPath: execPath, Namespace: "u1", Session: "s1", TempDir: t.TempDir()})
	defer b.Close()

	// upload：VFS 读 → 临时文件 → CLI
	res, err := b.Handle(context.Background(), env, "msg-1", []string{"upload", "@e5", "$SESSION/report.pdf"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Content != "uploaded 1 file(s) to @e5" {
		t.Errorf("upload content = %q", res.Content)
	}
	if !strings.Contains(readLog(t, logPath), "upload @e5") {
		t.Errorf("upload cli args = %q", readLog(t, logPath))
	}

	// download：CLI 落临时目录 → VFS 写，attrs path 为 VFS 路径
	res, err = b.Handle(context.Background(), env, "msg-1", []string{"download", "@e2", "report.csv"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Attrs["path"] != "/sessions/s1/report.csv" {
		t.Errorf("download attrs = %v", res.Attrs)
	}
	data, _ := vfs.ReadFile("/sessions/s1/report.csv")
	if strings.TrimSpace(string(data)) != "file-bytes" {
		t.Errorf("download vfs content = %q", data)
	}

	// screenshot：VFS 写 screenshot/ 下 + path（§2.2：不投递 image_data/image_path）
	res, err = b.Handle(context.Background(), env, "msg-1", []string{"screenshot", "--full"})
	if err != nil {
		t.Fatal(err)
	}
	p := res.Attrs["path"]
	if !strings.HasPrefix(p, "/sessions/s1/screenshot/") {
		t.Errorf("path = %q", p)
	}
	if res.Attrs["image_path"] != "" || res.Attrs["image_data"] != "" {
		t.Errorf("screenshot must not return image keys, attrs = %v", res.Attrs)
	}
	if data, _ := vfs.ReadFile(p); strings.TrimSpace(string(data)) != "jpeg-bytes" {
		t.Errorf("screenshot vfs content = %q", data)
	}
}

func TestBrowserPassthroughSubcommand(t *testing.T) {
	execPath, logPath := stubCLI(t)
	env := &vcore.Env{VFS: vcore.NewMemVFS(), Workdir: "/s"}
	b := New(Config{ExecPath: execPath, Session: "s1"})
	defer b.Close()

	// 非特化子命令透传 agent-browser（以 CLI 为唯一基准，不再白名单拒绝）
	res, err := b.Handle(context.Background(), env, "msg-1", []string{"type", "@e1", "hello"})
	if err != nil {
		t.Fatalf("passthrough type: %v", err)
	}
	if res == nil || res.Content == "" {
		t.Errorf("passthrough content empty")
	}
	if !strings.Contains(readLog(t, logPath), " type @e1 hello") {
		t.Errorf("CLI args = %q", readLog(t, logPath))
	}

	// network 子命令模式透传（route/unroute/request/har）
	if _, err := b.Handle(context.Background(), env, "msg-1", []string{"network", "route", "**/api/*", "--abort"}); err != nil {
		t.Fatalf("network route passthrough: %v", err)
	}
	if !strings.Contains(readLog(t, logPath), " network route **/api/* --abort") {
		t.Errorf("network route args = %q", readLog(t, logPath))
	}

	_, err = b.Handle(context.Background(), env, "msg-1", nil)
	if err == nil || !strings.Contains(err.Error(), "browser: subcommand is required") {
		t.Errorf("err = %v", err)
	}
}

// 安全：download 落盘越界拒绝（chroot 单根收容，§5.6：空间内任意路径放行，
// ".." 经 path.Clean 被钳在空间内，无法逃逸）。
func TestBrowserDownloadPathSandbox(t *testing.T) {
	execPath, _ := stubCLI(t)
	vfs := vcore.NewMemVFS()
	cloudEnv := &vcore.Env{
		VFS:     vfs,
		Workdir: "/",
		Roots:   []string{"/"},
	}
	b := New(Config{ExecPath: execPath, Namespace: "u1", Session: "s1", TempDir: t.TempDir()})
	defer b.Close()

	// 空间内：放行
	if _, err := b.Handle(context.Background(), cloudEnv, "msg-1", []string{"download", "@e1", "/a.bin"}); err != nil {
		t.Fatalf("in-space download rejected: %v", err)
	}
	// 路径穿越被 chroot 钳回空间内（/evil.bin），放行但不逃逸
	if _, err := b.Handle(context.Background(), cloudEnv, "msg-1", []string{"download", "@e1", "../../evil.bin"}); err != nil {
		t.Fatalf("traversal should be clamped into space: %v", err)
	}
	if _, err := vfs.Stat("/evil.bin"); err != nil {
		t.Fatalf("traversal target should land at /evil.bin: %v", err)
	}
}

// 安全：upload 源文件越界拒绝（chroot 单根收容）。
func TestBrowserUploadPathSandbox(t *testing.T) {
	execPath, _ := stubCLI(t)
	vfs := vcore.NewMemVFS()
	vfs.SetFile("/ok.txt", []byte("ok"), time.Now())
	cloudEnv := &vcore.Env{
		VFS:     vfs,
		Workdir: "/",
		Roots:   []string{"/"},
	}
	b := New(Config{ExecPath: execPath, Namespace: "u1", Session: "s1", TempDir: t.TempDir()})
	defer b.Close()

	if _, err := b.Handle(context.Background(), cloudEnv, "msg-1", []string{"upload", "@f", "/ok.txt"}); err != nil {
		t.Fatalf("in-space upload rejected: %v", err)
	}
	// 穿越被钳回空间内后文件不存在 → 执行错误（读不到内容）
	if _, err := b.Handle(context.Background(), cloudEnv, "msg-1", []string{"upload", "@f", "../../etc/passwd"}); err == nil {
		t.Fatal("upload of nonexistent in-space file accepted, want error")
	}
}

// 物理 host（Roots=nil）：路径不限制（用户机器自身边界，§2.6）。
func TestBrowserDownloadHostUnrestricted(t *testing.T) {
	execPath, _ := stubCLI(t)
	vfs := vcore.NewMemVFS()
	hostEnv := &vcore.Env{VFS: vfs, Workdir: "/workspace"} // Vars=nil, Roots=nil
	b := New(Config{ExecPath: execPath, Namespace: "u1", Session: "s1", TempDir: t.TempDir()})
	defer b.Close()

	if _, err := b.Handle(context.Background(), hostEnv, "msg-1", []string{"download", "@e1", "/tmp/any.bin"}); err != nil {
		t.Fatalf("host download should be unrestricted, got: %v", err)
	}
}

// 托管模式（§5.9）：CLI 子进程经 exec_procs 管理——输出落盘 LogPathFn 指定路径、
// 请求超时自动后台化（background attrs）、Content 为日志前 N 行。
func TestBrowserExecProcsManaged(t *testing.T) {
	execPath, _ := stubCLI(t)
	m := exec_procs.NewManager(0)
	logDir := t.TempDir()
	env := &vcore.Env{VFS: vcore.NewMemVFS(), Workdir: "/s"}
	b := New(Config{
		ExecPath:  execPath,
		Session:   "s1",
		TempDir:   t.TempDir(),
		ExecProcs: m,
		// 真实 host 路径即 NoSandbox（§5.6/§5.10），测试对齐；
		// 该用例验证托管机制（落盘/path/不后台），与沙箱无关。
		NoSandbox: true,
		LogPathFn: func(msgID string) string { return filepath.Join(logDir, msgID+".log") },
	})
	defer b.Close()

	// 正常完成：输出落盘 + path attrs
	res, err := b.Handle(context.Background(), env, "msg-1", []string{"snapshot"})
	if err != nil {
		t.Fatalf("managed snapshot: %v", err)
	}
	if res.Attrs["path"] != filepath.Join(logDir, "msg-1.log") {
		t.Errorf("path attrs = %q", res.Attrs["path"])
	}
	if data, _ := os.ReadFile(filepath.Join(logDir, "msg-1.log")); len(data) == 0 {
		t.Error("log file empty (no redirect)")
	}
	if res.Attrs["background"] == "true" {
		t.Error("snapshot should not background")
	}
}

// close --all 透传（回归：此前固定 "close" 丢弃 --all，会话未关闭）。
func TestBrowserClosePassesArgs(t *testing.T) {
	execPath, logPath := stubCLI(t)
	env := &vcore.Env{VFS: vcore.NewMemVFS(), Workdir: "/s"}
	b := New(Config{ExecPath: execPath, Session: "s1", TempDir: t.TempDir()})
	defer b.Close()

	if _, err := b.Handle(context.Background(), env, "msg-1", []string{"close", "--all"}); err != nil {
		t.Fatalf("close --all: %v", err)
	}
	if !strings.Contains(readLog(t, logPath), " close --all") {
		t.Errorf("close args = %q (--all dropped)", readLog(t, logPath))
	}
}

// pdf 导出沙箱（§5.9）：$SESSION 外拒绝、越界拒绝、$SESSION 内正常导出到 VFS。
func TestBrowserPdfPathSandbox(t *testing.T) {
	execPath, _ := stubCLI(t)
	vfs := vcore.NewMemVFS()
	cloudEnv := &vcore.Env{
		VFS:     vfs,
		Workdir: "/sessions/s1",
		Vars:    map[string]string{"$USER": "/home/u1", "$AGENT": "/agents/a1", "$SESSION": "/sessions/s1"},
		Roots:   []string{"/home/u1", "/agents/a1", "/sessions/s1"},
	}
	b := New(Config{ExecPath: execPath, Session: "s1", TempDir: t.TempDir()})
	defer b.Close()

	// 越界拒绝
	if _, err := b.Handle(context.Background(), cloudEnv, "msg-1", []string{"pdf", "/etc/x.pdf"}); err == nil {
		t.Fatal("pdf to /etc accepted, want reject")
	}
	// $SESSION 外拒绝
	if _, err := b.Handle(context.Background(), cloudEnv, "msg-1", []string{"pdf", "/home/u1/x.pdf"}); err == nil {
		t.Fatal("pdf to $USER accepted, want reject")
	}
	// 穿越拒绝
	if _, err := b.Handle(context.Background(), cloudEnv, "msg-1", []string{"pdf", "../../x.pdf"}); err == nil {
		t.Fatal("pdf traversal accepted, want reject")
	}
	// $SESSION 内正常导出（stub CLI 写临时文件 → VFS 落盘）
	res, err := b.Handle(context.Background(), cloudEnv, "msg-1", []string{"pdf", "/sessions/s1/a.pdf"})
	if err != nil {
		t.Fatalf("pdf in-session: %v", err)
	}
	if res.Attrs["path"] != "/sessions/s1/a.pdf" {
		t.Errorf("pdf path = %q", res.Attrs["path"])
	}
}

// close 后不得触发异步保存（§5.6：任何 CLI 调用都会唤起 daemon——
// close 后的 state save 会重启浏览器实例）。验证 1.5s 内无后续 CLI 调用。
func TestBrowserCloseNoRebirth(t *testing.T) {
	execPath, logPath := stubCLI(t)
	statePath := filepath.Join(t.TempDir(), "state.json")
	env := &vcore.Env{VFS: vcore.NewMemVFS(), Workdir: "/s"}
	b := New(Config{ExecPath: execPath, Session: "s1", TempDir: t.TempDir(), StatePath: statePath})
	defer b.Close()

	// 先制造脏 state（open 成功触发 markDirty）
	if _, err := b.Handle(context.Background(), env, "msg-1", []string{"open", "https://example.com"}); err != nil {
		t.Fatalf("open: %v", err)
	}
	// close --all：close 前应同步保存（log 里有一次 state save），close 后无任何调用
	if _, err := b.Handle(context.Background(), env, "msg-2", []string{"close", "--all"}); err != nil {
		t.Fatalf("close: %v", err)
	}
	time.Sleep(1500 * time.Millisecond) // 超过防抖窗口
	log := readLog(t, logPath)
	if strings.Count(log, "state save") != 1 {
		t.Errorf("state save calls = %d, want 1 (close-before only)\nlog: %s", strings.Count(log, "state save"), log)
	}
	if !strings.Contains(strings.TrimSpace(log), " close --all") {
		t.Errorf("last call should be close --all\nlog: %s", log)
	}
}
