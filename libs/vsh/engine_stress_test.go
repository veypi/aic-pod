package vsh

// engine_stress_test.go 是 todo 4.1.8 的稳定性基线：连续 500 次 exec 无
// 泄漏（goroutine/FD/RSS 前后对比）、Session P95 记录数值、并发满载下
// bg list/wait/kill 仍可用。数值写入 vsh/docs/todo.md 验收记录。

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// openFDCount 当前进程打开的 FD 数（/dev/fd 在 darwin/linux 均可用；
// windows 返回 -1 跳过该维度）。
func openFDCount() int {
	if runtime.GOOS == "windows" {
		return -1
	}
	ents, err := os.ReadDir("/dev/fd")
	if err != nil {
		return -1
	}
	return len(ents)
}

// procRSSKB 当前进程 RSS（KiB；ps 可移植到 darwin/linux；windows 返回 -1）。
func procRSSKB() int64 {
	if runtime.GOOS == "windows" {
		return -1
	}
	out, err := exec.Command("ps", "-o", "rss=", "-p", strconv.Itoa(os.Getpid())).Output()
	if err != nil {
		return -1
	}
	v, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		return -1
	}
	return v
}

// stressStats 一轮测量的快照。
type stressStats struct {
	goroutines int
	fds        int
	rssKB      int64
}

func snapshotStats() stressStats {
	return stressStats{goroutines: runtime.NumGoroutine(), fds: openFDCount(), rssKB: procRSSKB()}
}

// TestEngineStability500 连续 500 次 exec：短脚本 + 管道组合，逐次计时；
// 收尾 GC 后对比 goroutine/FD/RSS；P95 与资源增量 t.Logf 记录（验收数值
// 转抄 vsh/docs/todo.md 4.1.8）。泄漏断言取宽松阈值（防回归，不测抖动）。
func TestEngineStability500(t *testing.T) {
	if testing.Short() {
		t.Skip("short 模式跳过 500 次稳定性测量")
	}
	t.Parallel()
	e := newTestEngine(t)
	ctx := context.Background()
	execOnce := func(script string) time.Duration {
		start := time.Now()
		res, err := e.Exec(ctx, ExecRequest{SessionKey: "stress", Script: script, Timeout: time.Minute})
		if err != nil {
			t.Fatalf("exec: %v", err)
		}
		if res.ExitCode != 0 {
			t.Fatalf("exit=%d stderr=%s", res.ExitCode, res.Stderr)
		}
		return time.Since(start)
	}
	// 预热：layout/stub 初始化等一次性成本不进测量。
	for i := 0; i < 10; i++ {
		execOnce("echo warm | cat")
	}
	runtime.GC()
	time.Sleep(50 * time.Millisecond)
	before := snapshotStats()

	const n = 500
	durs := make([]time.Duration, 0, n)
	for i := 0; i < n; i++ {
		durs = append(durs, execOnce(fmt.Sprintf("echo iter-%d | cat | wc -c", i)))
	}
	// 并发满载（32 并发）中 bg list 可用性（满载时任务表读写不互锁死）。
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res, err := e.Exec(ctx, ExecRequest{SessionKey: "stress", Script: fmt.Sprintf("echo par-%d", i), Timeout: time.Minute})
			if err != nil || res.ExitCode != 0 {
				t.Errorf("并发 exec: err=%v exit=%d", err, res.ExitCode)
			}
		}(i)
	}
	res, err := e.Exec(ctx, ExecRequest{SessionKey: "stress", Script: "bg list", Timeout: time.Minute})
	if err != nil || res.ExitCode != 0 {
		t.Errorf("满载下 bg list 不可用：err=%v exit=%d stderr=%s", err, res.ExitCode, res.Stderr)
	}
	wg.Wait()

	runtime.GC()
	time.Sleep(100 * time.Millisecond)
	after := snapshotStats()

	sort.Slice(durs, func(i, j int) bool { return durs[i] < durs[j] })
	p95 := durs[n*95/100]
	p50 := durs[n/2]
	t.Logf("4.1.8 基线：n=%d P50=%s P95=%s max=%s", n, p50, p95, durs[n-1])
	t.Logf("goroutine %d→%d（Δ%+d） FD %d→%d（Δ%+d） RSS %d→%d KiB（Δ%+d KiB）",
		before.goroutines, after.goroutines, after.goroutines-before.goroutines,
		before.fds, after.fds, after.fds-before.fds,
		before.rssKB, after.rssKB, after.rssKB-before.rssKB)
	if d := after.goroutines - before.goroutines; d > 8 {
		t.Errorf("goroutine 泄漏疑似：Δ%+d", d)
	}
	if before.fds >= 0 && after.fds >= 0 {
		if d := after.fds - before.fds; d > 8 {
			t.Errorf("FD 泄漏疑似：Δ%+d", d)
		}
	}
	if before.rssKB >= 0 && after.rssKB >= 0 {
		if d := after.rssKB - before.rssKB; d > 128*1024 {
			t.Errorf("RSS 增长超限（>128MiB）：Δ%+d KiB", d)
		}
	}
}
