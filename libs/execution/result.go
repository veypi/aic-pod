package execution

// result.go 是 exec 结果的统一整形层（cloud/host 共用）：日志落盘收口、
// 预览截断、attrs 组装与 RTC 全量恢复。执行编排（wait.go）只产出
// ExecResult；把它变成 tool 响应是这里唯一的实现。

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/veypi/aic-pod/protocol"
)

// 预览行数上限（NATS/AI 消费）。跨仓共用同一常量，不再由各端手抄。
const (
	PreviewStdoutLines = 1000
	PreviewStderrLines = 100
)

// LogPair 拥有一次执行的两条惰性日志流与路径；stdout/stderr 直接作为引擎
// 的 stdio 接线。
type LogPair struct {
	Stdout  *LogSpiller
	Stderr  *LogSpiller
	OutPath string
	ErrPath string
}

// NewLogPair 构造执行日志对（不触碰文件系统，首次溢出/强制才建文件）。
func NewLogPair(outPath, errPath string) *LogPair {
	return &LogPair{
		Stdout:  NewLogSpiller(outPath, MaxStdoutBytes),
		Stderr:  NewLogSpiller(errPath, MaxStderrBytes),
		OutPath: outPath,
		ErrPath: errPath,
	}
}

// SpillAll 强制两条流落盘。
func (p *LogPair) SpillAll() error {
	return errors.Join(p.Stdout.Spill(), p.Stderr.Spill())
}

// AttachPaths 把已落盘的日志地址挂到 attrs（文件存在才挂）。
func (p *LogPair) AttachPaths(attrs map[string]string) {
	if p.Stdout.Spilled() {
		attrs["output"] = p.OutPath
	}
	if p.Stderr.Spilled() {
		attrs["error_output"] = p.ErrPath
	}
}

// LogSnapshot 记录关闭时截断流的实际字节数（RTC 全量恢复的前置校验）。
type LogSnapshot struct {
	StdoutBytes int64
	StderrBytes int64
	Err         error
}

// CloseResult 在执行真正结束时关闭两条流并记录截断流的最终大小。
// 必须在完成回调（onDone）里调用一次，且在 Output 之前。
func (p *LogPair) CloseResult(res *ExecResult) LogSnapshot {
	var snap LogSnapshot
	if res != nil {
		for _, stream := range []struct {
			sp        *LogSpiller
			truncated bool
			size      *int64
		}{
			{p.Stdout, res.StdoutTruncated, &snap.StdoutBytes},
			{p.Stderr, res.StderrTruncated, &snap.StderrBytes},
		} {
			if !stream.truncated {
				continue
			}
			size, err := stream.sp.Size()
			if err != nil {
				snap.Err = errors.Join(snap.Err, err)
			} else {
				*stream.size = size
			}
		}
	}
	snap.Err = errors.Join(snap.Err, p.Stdout.Close(), p.Stderr.Close())
	return snap
}

// Settle 校验落盘状态，并在 AI/NATS 预览路径下补齐行预览截断的日志：
// 引擎采集截断必须已溢出落盘（未落盘 = 输出尾部丢失，明确报错）；attrs
// 按「文件存在」挂 output/error_output。
func (p *LogPair) Settle(attrs map[string]string, res *ExecResult, preview bool) error {
	if err := errors.Join(p.Stdout.Err(), p.Stderr.Err()); err != nil {
		return protocol.Fail("internal", "exec: write log: "+err.Error())
	}
	if res.StdoutTruncated && !p.Stdout.Spilled() {
		return protocol.Fail("internal", "exec: stdout capture truncated without log spill")
	}
	if res.StderrTruncated && !p.Stderr.Spilled() {
		return protocol.Fail("internal", "exec: stderr capture truncated without log spill")
	}
	if preview {
		if _, cut := HeadLines(res.Stdout, PreviewStdoutLines); cut {
			if err := p.Stdout.Spill(); err != nil {
				return protocol.Fail("internal", "exec: spill log: "+err.Error())
			}
		}
		if _, cut := HeadLines(res.Stderr, PreviewStderrLines); cut {
			if err := p.Stderr.Spill(); err != nil {
				return protocol.Fail("internal", "exec: spill log: "+err.Error())
			}
		}
	}
	p.AttachPaths(attrs)
	return nil
}

// Captured 构造仅基于内存采集的响应（不做日志恢复）：full=true 直接返回
// 引擎采集串 + stderr（RTC 等待超时/容量取消等执行未终结场景）；full=false
// 为 AI/NATS 有界预览。
func (p *LogPair) Captured(res *ExecResult, attrs map[string]string, full bool) *protocol.Output {
	attrs["exit_code"] = strconv.Itoa(res.ExitCode)
	if !full {
		return p.preview(res, attrs)
	}
	if res.Stderr != "" {
		attrs["stderr"] = res.Stderr
	}
	return &protocol.Output{Content: res.Stdout, Attrs: attrs}
}

// Completed 构造执行真正结束后的响应：full=false 为 AI/NATS 预览；full=true
// 为 RTC 直连全量——截断流从本次执行已关闭的日志恢复，超出 RTC 上限或
// 恢复失败明确报错。成功与失败都保留 attrs 元数据；失败不返回半截 payload。
func (p *LogPair) Completed(res *ExecResult, attrs map[string]string, full bool, snap LogSnapshot) (*protocol.Output, error) {
	attrs["exit_code"] = strconv.Itoa(res.ExitCode)
	if !full {
		return p.preview(res, attrs), nil
	}
	fail := func(code, message string) (*protocol.Output, error) {
		delete(attrs, "stderr")
		return &protocol.Output{Attrs: attrs}, protocol.Fail(code, "exec: "+message)
	}
	if snap.Err != nil {
		return fail("internal", "finish output logs: "+snap.Err.Error())
	}
	stdoutBytes, stderrBytes := int64(len(res.Stdout)), int64(len(res.Stderr))
	if res.StdoutTruncated {
		stdoutBytes = snap.StdoutBytes
	}
	if res.StderrTruncated {
		stderrBytes = snap.StderrBytes
	}
	if stdoutBytes < int64(len(res.Stdout)) || stderrBytes < int64(len(res.Stderr)) {
		return fail("internal", "complete output log is shorter than the captured output")
	}
	if stdoutBytes < 0 || stderrBytes < 0 || stdoutBytes > protocol.RtcToolResponseLimit || stderrBytes > protocol.RtcToolResponseLimit-stdoutBytes {
		return fail("overloaded", fmt.Sprintf("output exceeds %d bytes; full output is available in the execution logs", protocol.RtcToolResponseLimit))
	}
	// 拷贝结果：恢复输出不得改动引擎/任务表共享的结果。
	fullRes := *res
	for _, stream := range []struct {
		path      string
		truncated bool
		size      int64
		value     *string
	}{
		{p.OutPath, res.StdoutTruncated, stdoutBytes, &fullRes.Stdout},
		{p.ErrPath, res.StderrTruncated, stderrBytes, &fullRes.Stderr},
	} {
		if !stream.truncated {
			continue
		}
		file, err := os.Open(stream.path)
		if err != nil {
			return fail("internal", "read complete output log: "+err.Error())
		}
		data := make([]byte, int(stream.size))
		_, readErr := io.ReadFull(file, data)
		closeErr := file.Close()
		if err := errors.Join(readErr, closeErr); err != nil {
			return fail("internal", "read complete output log: "+err.Error())
		}
		*stream.value = string(data)
	}
	fullRes.StdoutTruncated, fullRes.StderrTruncated = false, false
	if fullRes.Stderr != "" {
		attrs["stderr"] = fullRes.Stderr
	}
	return &protocol.Output{Content: fullRes.Stdout, Attrs: attrs}, nil
}

// preview 是有界 AI/NATS 视图（exit_code 已由调用方设置）。
func (p *LogPair) preview(res *ExecResult, attrs map[string]string) *protocol.Output {
	truncated := res.StdoutTruncated || res.StderrTruncated
	content, cut := HeadLines(res.Stdout, PreviewStdoutLines)
	truncated = truncated || cut
	if res.Stderr != "" {
		errHead, errCut := HeadLines(res.Stderr, PreviewStderrLines)
		attrs["stderr"] = errHead
		truncated = truncated || errCut
	}
	if truncated {
		attrs["truncated"] = "true"
	}
	return &protocol.Output{Content: content, Attrs: attrs}
}

// HeadLines 截取前 n 行；发生截取时返回 truncated=true。按原始字节保留
// 行尾（与 aic 使用同一实现，避免两端各写一份 Scanner 语义）。
func HeadLines(s string, n int) (string, bool) {
	if n <= 0 || s == "" {
		return s, false
	}
	lines := strings.SplitAfter(s, "\n")
	if len(lines) <= n {
		return s, false
	}
	return strings.Join(lines[:n], ""), true
}
