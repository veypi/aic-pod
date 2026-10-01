package host

// skill 包 stream 端点 → RTC tool.Stream 的桥接（v6 P3，docs/skill.md §9.2）。
// tool.Stream 是消息语义（Recv 一条收一条）；skillrun.Stream 底层 skillproc
// 帧天然保边界——桥接用 ReadFrame/Write 一帧对一条，不展平为字节流。
// 读侧单 pump goroutine（Recv 的 ctx 可取消；Close 关连解锁全部等待者）。

import (
	"context"
	"io"
	"sync"

	"github.com/veypi/aic-pod/libs/skillrun"
)

// skillToolStream 实现 tool.Stream（hosts_tool.Stream 别名 wire.Stream）。
type skillToolStream struct {
	s      *skillrun.Stream
	frames chan skillFrame
	done   chan struct{}
	once   sync.Once
}

type skillFrame struct {
	b   []byte
	err error
}

func newSkillToolStream(s *skillrun.Stream) *skillToolStream {
	st := &skillToolStream{s: s, frames: make(chan skillFrame, 16), done: make(chan struct{})}
	go st.pump()
	return st
}

func (st *skillToolStream) pump() {
	for {
		b, err := st.s.ReadFrame()
		select {
		case st.frames <- skillFrame{b: b, err: err}:
		case <-st.done:
			return
		}
		if err != nil {
			return
		}
	}
}

// Recv 收一条消息。ctx 取消只放弃本次等待（流仍可续读——未取帧在队列里）；
// 流结束/断开 = 读帧错误原样返回（io.EOF 即对端关闭）。
func (st *skillToolStream) Recv(ctx context.Context) ([]byte, error) {
	select {
	case f := <-st.frames:
		return f.b, f.err
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-st.done:
		return nil, io.EOF
	}
}

// Send 发一条消息 = 一帧。本地 unix socket 写近似非阻塞（无流控协议），
// ctx/关闭仅做写前预检。
func (st *skillToolStream) Send(ctx context.Context, b []byte) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-st.done:
		return io.EOF
	default:
	}
	_, err := st.s.Write(b)
	return err
}

// Close 关闭底层通道（幂等；pump 与等待中的 Recv 经 done/断连解锁）。
func (st *skillToolStream) Close() error {
	st.once.Do(func() {
		close(st.done)
		_ = st.s.Close()
	})
	return nil
}
