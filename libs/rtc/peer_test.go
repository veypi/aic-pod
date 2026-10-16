package rtc

import "testing"

// 流发送锁 per-label：同标签同锁、异标签异锁（原全局 streamSend 单锁下，
// 一个拥塞通道可把全部流通道阻塞到 30s 超时）。
func TestStreamLockPerLabel(t *testing.T) {
	p := &peer{}
	a1, a2, b := p.streamLock("aic-a"), p.streamLock("aic-a"), p.streamLock("aic-b")
	if a1 != a2 {
		t.Fatal("同标签未复用同一把锁")
	}
	if a1 == b {
		t.Fatal("异标签共用了一把锁")
	}
}
