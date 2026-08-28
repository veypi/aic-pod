//go:build darwin

package exec_procs

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/veypi/aic-pod/libs/proto"
)

// groupRSSKB 对自己进程组返回非零 RSS（进程本身必有物理内存）。
// 注：CI/沙箱环境可能禁止 fork ps（operation not permitted），skip。
func TestGroupRSSKB(t *testing.T) {
	kb, err := groupRSSKB(os.Getpid())
	if err != nil {
		if strings.Contains(err.Error(), "operation not permitted") {
			t.Skipf("environment forbids ps: %v", err)
		}
		t.Fatalf("groupRSSKB: %v", err)
	}
	if kb == 0 {
		t.Fatal("groupRSSKB(self) = 0, want > 0")
	}
}

// rssLimitBytes：非零且在 [1GiB, 8GiB]（min(8GiB, mem/2)）。
func TestRssLimitBytes(t *testing.T) {
	limit := rssLimitBytes()
	if limit < 1<<30 || limit > 8<<30 {
		t.Fatalf("rssLimitBytes = %d, want in [1GiB, 8GiB]", limit)
	}
}

// 集成：正常进程（分配 ~200MB，远低于上限）经沙箱 Start 不被 RSS 监控
// 误杀，exit 0 且输出正确。嵌套沙箱环境（无后端 fail-closed）skip。
func TestMonitorDoesNotKillNormalProcess(t *testing.T) {
	m := NewManager(time.Minute)
	res, err := m.Start(context.Background(), StartOptions{
		ID:      "t-rss-ok",
		Command: "python alloc 200MB",
		LogPath: filepath.Join(t.TempDir(), "out.log"),
		Level:   proto.LevelRead,
		Exec:    []string{"python3", "-c", "import time; x=bytearray(200*1024*1024); time.sleep(0.2); print(len(x))"},
	})
	if err != nil {
		if strings.Contains(err.Error(), "no sandbox backend") {
			t.Skipf("nested sandbox unavailable: %v", err)
		}
		t.Fatalf("start: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("exit = %d, want 0 (should not be killed): %s", res.ExitCode, res.Content)
	}
	if !strings.Contains(res.Content, "209715200") {
		t.Fatalf("output missing alloc size: %q", res.Content)
	}
}
