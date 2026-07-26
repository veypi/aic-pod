package aichost

import "sync"

type idempotentCache struct {
	mu    sync.Mutex
	store map[string]*toolResponse
}

func (c *idempotentCache) get(key string) *toolResponse {
	if key == "" {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.store[key]
}

func (c *idempotentCache) set(key string, resp *toolResponse) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.store == nil {
		c.store = make(map[string]*toolResponse)
	}
	if len(c.store) > 1024 {
		c.store = make(map[string]*toolResponse)
	}
	c.store[key] = resp
}
