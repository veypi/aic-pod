package host

import (
	"context"
	"io"
	"os"
	"runtime"
	"testing"

	"github.com/veypi/aic-pod/libs/exec_procs"
	"github.com/veypi/aic-pod/libs/proto"
)

// 嵌套执行（exec_procs.Output 非空，平台命令的常态路径）不经 StartCall，
// 必须自行确保会话工作区存在：windows 沙箱授予可写根要求目录已存在，
// 否则会话内首次执行报 "grant workspace ... cannot find the file"（回归）。
func TestNestedRunProcessEnsuresSessionWorkspace(t *testing.T) {
	c, _ := testClient(t)
	sid := "sess_nested"
	dir := c.sessionWorkDir(sid)
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("precondition: %s already exists", dir)
	}
	prog := []string{"sh", "-c", "exit 0"}
	if runtime.GOOS == "windows" {
		prog = []string{"cmd", "/c", "exit 0"}
	}
	ctx := exec_procs.WithOutput(context.Background(), io.Discard)
	resp := c.runProcess(ctx, sid, "m1", "probe", prog, t.TempDir(), proto.LevelWrite, true)
	if resp.State != proto.StateCompleted {
		t.Fatalf("nested runProcess state=%s error=%s", resp.State, resp.Error)
	}
	st, err := os.Stat(dir)
	if err != nil || !st.IsDir() {
		t.Fatalf("session workspace not created for nested execution: %v", err)
	}
}
