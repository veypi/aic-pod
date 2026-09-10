package vcore

import (
	"io"
	"io/fs"

	"github.com/veypi/vigo/contrib/ufs"
)

// MaxReadBinBytes 是 ReadBin 单次返回的字节上限（256MB，与前端预览的
// host 直连媒体上限一致）：readbin 为整文件驻内存通道，超限明确报错。
const MaxReadBinBytes = 256 << 20

// ReadBin 按字节区间读取文件原始字节（2026-09-10，RTC 直连通道 readbin op
// 的执行体）：**不属于 fs 指令集**（8 action 契约不变，AI 面不可见），仅经
// owner 直连控制台暴露——预览/下载大二进制（视频等）的字节出口。
//
// 权限门与 fs read 同一判定实例：Resolve → CheckPath → CheckPolicy（read 级，
// deny 恒拒不可绕过）。length<=0 = 读到文件尾；请求区间超 MaxReadBinBytes
// 报错（不静默截断——调用方拿残缺的视频无法播放，宁可显式失败）。
// mime 一律按文件头 512 字节探测（off>0 时单独读头，不用区间内数据猜）。
func ReadBin(env *Env, path string, off, length int64) (data []byte, mime string, total int64, err error) {
	if path == "" {
		return nil, "", 0, fsErr("readbin", "path is required")
	}
	if off < 0 {
		return nil, "", 0, fsErr("readbin", "off must be >= 0, got %d", off)
	}
	if length < 0 {
		return nil, "", 0, fsErr("readbin", "len must be >= 0, got %d", length)
	}
	abs, err := env.Resolve(path)
	if err != nil {
		return nil, "", 0, fsErr("readbin", "%s", err)
	}
	if err := env.CheckPath("readbin", abs); err != nil {
		return nil, "", 0, err
	}
	if err := env.CheckPolicy("readbin", abs, false); err != nil {
		return nil, "", 0, err
	}
	info, err := env.VFS.Stat(abs)
	if err != nil {
		return nil, "", 0, fsErr("readbin", "%s", err)
	}
	if info.IsDir() {
		return nil, "", 0, fsErr("readbin", "%s is a directory", abs)
	}
	total = info.Size()
	if off > total {
		return nil, "", 0, fsErr("readbin", "off %d exceeds file size %d", off, total)
	}
	want := length
	if want <= 0 || off+want > total {
		want = total - off
	}
	if want > MaxReadBinBytes {
		return nil, "", 0, fsErr("readbin", "requested %d bytes exceeds max %d", want, MaxReadBinBytes)
	}

	f, err := env.VFS.Open(abs)
	if err != nil {
		return nil, "", 0, fsErr("readbin", "%s", err)
	}
	defer f.Close()
	// 区间定位：底层支持 Seek 直接跳（OS 文件）；否则整读切片（memfs 等）。
	if sk, ok := f.(io.Seeker); ok {
		if _, err := sk.Seek(off, io.SeekStart); err != nil {
			return nil, "", 0, fsErr("readbin", "seek: %s", err)
		}
		data, err = readExact(f, want)
	} else {
		var all []byte
		all, err = readAllCapped(f, off+want)
		if err == nil {
			if int64(len(all)) < off {
				all = nil
			} else {
				all = all[off:]
			}
			data = all
		}
	}
	if err != nil {
		return nil, "", 0, fsErr("readbin", "%s", err)
	}

	// mime 按文件头探测：off=0 时复用已读数据，否则单独补读头部。
	head := data
	if off > 0 || len(head) == 0 {
		head = readHead(env.VFS, abs, 512)
	} else if len(head) > 512 {
		head = head[:512]
	}
	return data, detectMIME(head, abs), total, nil
}

// readExact 精确读 n 字节（不足 n 返回实际读到的，无错——并发截断容忍）。
func readExact(f fs.File, n int64) ([]byte, error) {
	buf := make([]byte, n)
	read, err := io.ReadFull(f, buf)
	if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
		return nil, err
	}
	return buf[:read], nil
}

// readAllCapped 读到 limit 字节为止（无 Seek 后端的整读兜底，仍不超额）。
func readAllCapped(f fs.File, limit int64) ([]byte, error) {
	return io.ReadAll(io.LimitReader(f, limit))
}

// readHead 读文件头 n 字节（mime 探测用；失败返回 nil，调用方按扩展名兜底）。
func readHead(vfs ufs.FS, abs string, n int) []byte {
	f, err := vfs.Open(abs)
	if err != nil {
		return nil
	}
	defer f.Close()
	buf := make([]byte, n)
	read, _ := io.ReadFull(f, buf)
	return buf[:read]
}
