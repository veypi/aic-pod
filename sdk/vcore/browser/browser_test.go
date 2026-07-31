package browser

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/veypi/aic-pod/sdk/vcore"
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

	res, err := b.Handle(context.Background(), env, []string{"open", "https://example.com"})
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
	res, err := b.Handle(context.Background(), env, []string{"upload", "@e5", "$SESSION/report.pdf"})
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
	res, err = b.Handle(context.Background(), env, []string{"download", "@e2", "report.csv"})
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

	// screenshot：VFS 写 screenshot/ 下 + image_path
	res, err = b.Handle(context.Background(), env, []string{"screenshot", "--full"})
	if err != nil {
		t.Fatal(err)
	}
	p := res.Attrs["image_path"]
	if !strings.HasPrefix(p, "/sessions/s1/screenshot/") {
		t.Errorf("image_path = %q", p)
	}
	if data, _ := vfs.ReadFile(p); strings.TrimSpace(string(data)) != "jpeg-bytes" {
		t.Errorf("screenshot vfs content = %q", data)
	}
}

func TestBrowserUnknownSubcommand(t *testing.T) {
	execPath, _ := stubCLI(t)
	env := &vcore.Env{VFS: vcore.NewMemVFS(), Workdir: "/s"}
	b := New(Config{ExecPath: execPath, Session: "s1"})
	defer b.Close()
	_, err := b.Handle(context.Background(), env, []string{"var", "list"})
	if err == nil || !strings.Contains(err.Error(), `browser: unknown subcommand "var"`) {
		t.Errorf("err = %v", err)
	}
	_, err = b.Handle(context.Background(), env, nil)
	if err == nil || !strings.Contains(err.Error(), "browser: subcommand is required") {
		t.Errorf("err = %v", err)
	}
}
