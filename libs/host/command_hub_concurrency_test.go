package host

import (
	"sync"
	"testing"

	"github.com/veypi/aic-pod/cfg"
)

// TestCommandHubRuntimeMapAccessIsSerialized 守住 runtime map 的锁纪律。
//
// runtime map 的写方（upsertRuntime / removeRuntime）都在 rebuildMu 下，读方
// 必须同锁：并发 commands add / commands remove 撞上一次无锁读，会触发 Go
// 运行时的 concurrent map read and map write——那是不可 recover 的致命错误，
// 整个 pod 进程直接死（不是普通 panic）。
//
// 用例本身的价值在于「跑完不炸」：Go 运行时的 map 竞争检测是致命的，测试进程
// 被带走即失败；必须带 -race 跑（无 -race 时运行时 fatal 是概率事件，-race 下
// 由 race detector 稳定报出读写竞争）。
func TestCommandHubRuntimeMapAccessIsSerialized(t *testing.T) {
	c, _ := testClient(t)
	hub := c.cmds
	if hub == nil {
		t.Fatal("command hub not initialized")
	}
	def := &hostCommand{
		ID: "config:hub-race", Alias: "hub-race", Source: commandSourceConfig,
		Config: cfg.CommandConfig{Run: []string{"echo"}},
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(3)
		go func() { defer wg.Done(); _ = hub.upsertRuntime(def) }()
		go func() { defer wg.Done(); _ = hub.removeRuntime(def.ID) }()
		go func() { defer wg.Done(); _ = hub.hasRuntime(def.ID) }()
	}
	wg.Wait()
}
