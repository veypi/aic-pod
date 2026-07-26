package aichost

import (
	"sync"
	"time"
)

// replayCache 是防重放 nonce 窗口缓存（§10.1 第 3 条）：
// 同一 nonce 在 deadline 窗口内只允许使用一次，缓存条目于 deadline 过期后淘汰。
type replayCache struct {
	mu    sync.Mutex
	store map[string]time.Time // nonce → deadline
}

// checkAndMark 检查 nonce 是否已使用；未使用则标记并返回 true。
// deadline 为零值时按默认窗口（10 分钟）保留。
func (c *replayCache) checkAndMark(nonce string, deadline time.Time) bool {
	if nonce == "" {
		return true
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	// 惰性淘汰过期条目
	for n, dl := range c.store {
		if now.After(dl) {
			delete(c.store, n)
		}
	}
	if c.store == nil {
		c.store = make(map[string]time.Time)
	}
	if _, seen := c.store[nonce]; seen {
		return false
	}
	if deadline.IsZero() {
		deadline = now.Add(10 * time.Minute)
	}
	c.store[nonce] = deadline
	return true
}
