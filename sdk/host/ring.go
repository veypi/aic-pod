package host

import (
	"regexp"
	"strings"
	"sync"
)

// RingBuffer 是环形日志缓冲（io.Writer）：作为 vigo/logv 的输出 writer 之一，
// LocalAPI 的 get_log 从其中读取尾部内容（剥离 ANSI 颜色码）。
// 日志系统统一走 vigo/logv，本类型只承担「最近 N 行」的暂存职责。
type RingBuffer struct {
	mu      sync.Mutex
	lines   []string
	max     int
	pending string
}

var ansiRe = regexp.MustCompile(`\x1b\[[0-9;]*m`)

// NewRingBuffer 创建保留最近 max 行的环形缓冲。
func NewRingBuffer(max int) *RingBuffer {
	if max <= 0 {
		max = 200
	}
	return &RingBuffer{max: max}
}

// Write 实现 io.Writer：按行切分存储，丢弃超限旧行（剥离 ANSI 颜色码）。
func (r *RingBuffer) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pending += ansiRe.ReplaceAllString(string(p), "")
	for {
		i := strings.IndexByte(r.pending, '\n')
		if i < 0 {
			break
		}
		line := r.pending[:i]
		r.pending = r.pending[i+1:]
		if line != "" {
			r.append(line)
		}
	}
	return len(p), nil
}

func (r *RingBuffer) append(line string) {
	r.lines = append(r.lines, line)
	if len(r.lines) > r.max {
		r.lines = r.lines[len(r.lines)-r.max:]
	}
}

// Content 返回缓冲内容（尾部最多 max 行，按行连接）。
func (r *RingBuffer) Content() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Join(r.lines, "\n")
}
