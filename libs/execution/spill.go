package execution

// spill.go 是 exec 日志的惰性落盘 writer（cloud/host 接入层共用）。
//
// 语义（2026-10-07 用户裁定）：输出先留在内存缓冲，仅在两种情况下创建
// 日志文件——
//   - 溢出：缓冲超过 capBytes（对齐引擎采集上限 MaxStdoutBytes/
//     MaxStderrBytes；引擎采集截断 ⟹ 必已溢出落盘，tee 在采集截断后仍
//     继续写 writer，文件始终是全量）；
//   - 强制：调用方 Spill()——执行晚于响应返回（转后台/前台等待超时/断连
//     保留/容量取消）或同步完成时行预览截断（全量在内存，补建文件）。
//
// 同步完成且未截断的执行不产生任何文件：attrs 不挂 output/error_output
//（有路径 ⟺ 有更多内容）。执行可能晚于响应返回，Write 与 Spill/Close
// 并发安全。落盘失败是明确错误（Err() 非空，不静默降级）。
//
// 分层：本类型只是 io.Writer 工具——不命名、不决定何时落盘（路径与
// 时机都归调用方）；引擎经 ExecRequest.Stdout/Stderr 只写。

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
)

// LogSpiller 惰性落盘 writer：缓冲 ≤capBytes 的输出，溢出或强制时建文件
// 并刷入缓冲前缀，之后直写文件。
type LogSpiller struct {
	path     string
	capBytes int

	mu      sync.Mutex
	buf     []byte   // 未落盘前缀（spilled 后恒 nil）
	file    *os.File // spilled 且未关闭时非空
	spilled bool
	closed  bool  // 执行实际结束（Close 后仍可 Spill 补建文件）
	err     error // 首次落盘/写入/关闭失败（粘性）
}

// NewLogSpiller 返回以 path 为落盘目标的 writer；capBytes 为内存缓冲上限
// （超过即溢出落盘）。构造不触碰文件系统。
func NewLogSpiller(path string, capBytes int) *LogSpiller {
	return &LogSpiller{path: path, capBytes: capBytes}
}

// Write 实现 io.Writer（引擎 tee 的写入端）。
func (s *LogSpiller) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return 0, s.err
	}
	if s.spilled {
		if s.file == nil {
			// Close 后不应再有写（执行已结束）；防御性拒绝。
			return 0, os.ErrClosed
		}
		return s.file.Write(p)
	}
	if len(s.buf)+len(p) > s.capBytes {
		if err := s.spillLocked(); err != nil {
			return 0, err
		}
		return s.file.Write(p)
	}
	s.buf = append(s.buf, p...)
	return len(p), nil
}

// Spill 强制落盘：创建文件（含父目录）并刷入已缓冲前缀，后续 Write 直写
// 文件。幂等；执行已结束（Close 后）调用同样有效——写完即关。
func (s *LogSpiller) Spill() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	if !s.spilled {
		if err := s.spillLocked(); err != nil {
			return err
		}
	}
	if s.closed && s.file != nil {
		if err := s.file.Close(); err != nil {
			s.err = errors.Join(s.err, err)
		}
		s.file = nil
	}
	return s.err
}

func (s *LogSpiller) spillLocked() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		s.err = err
		return err
	}
	f, err := os.OpenFile(s.path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		s.err = err
		return err
	}
	if len(s.buf) > 0 {
		if _, err := f.Write(s.buf); err != nil {
			_ = f.Close()
			s.err = err
			return err
		}
		s.buf = nil
	}
	s.file = f
	s.spilled = true
	return nil
}

// Spilled 报告文件是否已落盘且可用（落盘失败 = false + Err 非空）。
func (s *LogSpiller) Spilled() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.spilled && s.err == nil
}

// Err 返回首个落盘/写入/关闭错误（无 = nil）。
func (s *LogSpiller) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

// Size 返回输出字节数：已落盘 = 文件大小（含执行中，os.File 写无缓冲）；
// 未落盘 = 缓冲长度。供截断流的全量恢复前置校验。
func (s *LogSpiller) Size() (int64, error) {
	s.mu.Lock()
	spilled, path, n := s.spilled, s.path, len(s.buf)
	s.mu.Unlock()
	if !spilled {
		return int64(n), nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}

// Close 在执行实际结束时调用一次：关闭已创建文件；未落盘的缓冲保留
// （调用方在同步完成后仍可 Spill 补建文件）。幂等。
func (s *LogSpiller) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return s.err
	}
	s.closed = true
	if s.file != nil {
		if err := s.file.Close(); err != nil {
			s.err = errors.Join(s.err, err)
		}
		s.file = nil
	}
	// 未落盘的缓冲保留：同步完成的行预览截断收口在 Close 之后 Spill 补建
	// 文件（spillLocked 刷入缓冲后置 nil，正常路径随后由 GC 回收）。
	return s.err
}
