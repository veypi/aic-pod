package hostfs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	fsp "github.com/veypi/aic-pod/protocol/fs"
	"io"
	"os"
	"strings"
	"time"
)

const RangeBytes = 24 << 10 // Fits a 64 KiB RTC message after JSON/base64 framing.
type TransferConfig struct {
	MaxUploadBytes, ProxyUploadBytes int64
	MaxSources                       int
}
type SourceArgs struct {
	Ref fsp.ResourceRef `json:"ref" required:"true"`
}
type ReadRangeArgs struct {
	Ref    fsp.ResourceRef `json:"ref" required:"true"`
	Offset int64           `json:"offset"`
	Length int64           `json:"length"`
}
type WriteRangeArgs struct {
	Ref    fsp.ResourceRef `json:"ref" required:"true"`
	Offset int64           `json:"offset"`
	Data   []byte          `json:"data"`
}
type UploadArgs struct {
	Size      int64  `json:"size"`
	SHA256    string `json:"sha256,omitempty"`
	MediaType string `json:"media_type,omitempty"`
}
type SealArgs struct {
	Ref    fsp.ResourceRef `json:"ref" required:"true"`
	SHA256 string          `json:"sha256,omitempty"`
}

// handleBytes 处理字节源/上传方法（数据面直调，MCP 不再有
// 方法声明层）。ok=false 表示非本组方法（交回 FS.Run）。
func (f *FS) handleBytes(ctx context.Context, call Call) (value any, ok bool, err error) {
	switch call.Method {
	case "source.describe":
		var a SourceArgs
		if err := fsp.Decode(call.Args, &a); err != nil {
			return nil, true, err
		}
		v, err := f.cfg.Bytes.Describe(call.Owner, a.Ref)
		return v, true, err
	case "source.release":
		var a SourceArgs
		if err := fsp.Decode(call.Args, &a); err != nil {
			return nil, true, err
		}
		err := f.cfg.Bytes.Release(call.Owner, a.Ref)
		return map[string]bool{"released": err == nil}, true, err
	case "source.read":
		var a ReadRangeArgs
		if err := fsp.Decode(call.Args, &a); err != nil {
			return nil, true, err
		}
		if a.Length < 0 || a.Length > RangeBytes {
			return nil, true, fsp.Fail("invalid_argument", "Range exceeds transfer limit")
		}
		var out bytes.Buffer
		err := f.cfg.Bytes.Copy(ctx, call.Owner, a.Ref, a.Offset, &a.Length, &out)
		return map[string]any{"data": out.Bytes(), "offset": a.Offset}, true, err
	case "upload.open":
		var a UploadArgs
		if err := fsp.Decode(call.Args, &a); err != nil {
			return nil, true, err
		}
		f.mu.Lock()
		limit := f.cfg.MaxProxyUploadBytes
		f.mu.Unlock()
		if call.Caller.Scope == "fs" && limit > 0 && a.Size > limit {
			return nil, true, fsp.Fail("overloaded", "Proxy upload quota")
		}
		v, err := f.cfg.Bytes.beginUpload(call.Owner, a)
		return v, true, err
	case "upload.write":
		var a WriteRangeArgs
		if err := fsp.Decode(call.Args, &a); err != nil {
			return nil, true, err
		}
		offset, err := f.cfg.Bytes.writeRange(ctx, call.Owner, a)
		return map[string]int64{"offset": offset}, true, err
	case "upload.seal":
		var a SealArgs
		if err := fsp.Decode(call.Args, &a); err != nil {
			return nil, true, err
		}
		v, err := f.cfg.Bytes.sealUpload(ctx, call.Owner, a)
		return v, true, err
	}
	return nil, false, nil
}
func (b *Bytes) beginUpload(owner string, a UploadArgs) (ByteSource, error) {
	if a.Size < 0 || a.Size > b.sourceLimit() {
		return ByteSource{}, fsp.Fail("overloaded", "Upload exceeds byte quota")
	}
	if a.SHA256 != "" {
		if raw, err := hex.DecodeString(a.SHA256); err != nil || len(raw) != 32 {
			return ByteSource{}, fsp.Fail("invalid_argument", "Invalid SHA-256")
		}
	}
	b.Reap()
	if err := b.reservation(owner); err != nil {
		return ByteSource{}, err
	}
	defer func() { b.mu.Lock(); b.finishReservation(owner); b.mu.Unlock() }()
	if err := b.reserveBytes(a.Size); err != nil {
		return ByteSource{}, err
	}
	file, err := os.CreateTemp(b.dir, "upload-")
	if err != nil {
		b.mu.Lock()
		b.used -= a.Size
		b.mu.Unlock()
		return ByteSource{}, err
	}
	id, _ := fsp.NewID("src_")
	desc := ByteSource{Ref: fsp.ResourceRef{ID: id, Epoch: b.epoch, Kind: "bytes"}, Size: a.Size, MediaType: a.MediaType, Seekable: true, Lifetime: "fs_source", SHA256: strings.ToLower(a.SHA256)}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		file.Close()
		os.Remove(file.Name())
		b.used -= a.Size
		return ByteSource{}, fsp.Fail("expired", "Byte store closed")
	}
	b.sources[id] = &byteSource{owner: owner, descriptor: desc, file: file, temp: file.Name(), expires: time.Now().Add(30 * time.Minute)}
	return desc, nil
}
func (b *Bytes) writeRange(ctx context.Context, owner string, a WriteRangeArgs) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if len(a.Data) > RangeBytes || a.Offset < 0 {
		return 0, fsp.Fail("invalid_argument", "Invalid upload range")
	}
	source, err := b.source(owner, a.Ref)
	if err != nil {
		return 0, err
	}
	source.mu.Lock()
	defer source.mu.Unlock()
	if source.closed || source.descriptor.Immutable || source.temp == "" {
		return 0, fsp.Fail("invalid_argument", "Upload is not writable")
	}
	if int64(len(a.Data)) > source.descriptor.Size-a.Offset {
		return 0, fsp.Fail("invalid_argument", "Upload range exceeds declared size")
	}
	if a.Offset < source.received {
		existing := make([]byte, len(a.Data))
		n, err := source.file.ReadAt(existing, a.Offset)
		if n == len(existing) && err == nil && bytes.Equal(existing, a.Data) {
			return source.received, nil
		}
		return 0, fsp.Fail("conflict", "Repeated upload range has different data")
	}
	if a.Offset != source.received {
		return 0, fsp.Fail("conflict", "Upload offset does not match received bytes")
	}
	n, err := source.file.WriteAt(a.Data, a.Offset)
	source.received += int64(n)
	return source.received, err
}
func (b *Bytes) sealUpload(ctx context.Context, owner string, a SealArgs) (ByteSource, error) {
	source, err := b.source(owner, a.Ref)
	if err != nil {
		return ByteSource{}, err
	}
	source.mu.Lock()
	defer source.mu.Unlock()
	if source.closed {
		return ByteSource{}, fsp.Fail("expired", "Upload expired")
	}
	if source.descriptor.Immutable {
		if a.SHA256 != "" && strings.ToLower(a.SHA256) != source.descriptor.SHA256 {
			return ByteSource{}, fsp.Fail("conflict", "SHA-256 mismatch")
		}
		return source.descriptor, nil
	}
	if source.temp == "" || source.received != source.descriptor.Size {
		return ByteSource{}, fsp.Fail("invalid_argument", "Upload incomplete")
	}
	hash := sha256.New()
	reader := io.NewSectionReader(source.file, 0, source.received)
	buf := make([]byte, 64<<10)
	for {
		if err := ctx.Err(); err != nil {
			return ByteSource{}, err
		}
		n, err := reader.Read(buf)
		if n > 0 {
			hash.Write(buf[:n])
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return ByteSource{}, err
		}
		if n == 0 {
			return ByteSource{}, io.ErrNoProgress
		}
	}
	sum := fmt.Sprintf("%x", hash.Sum(nil))
	expected := source.descriptor.SHA256
	if (expected != "" && sum != expected) || (a.SHA256 != "" && sum != strings.ToLower(a.SHA256)) {
		return ByteSource{}, fsp.Fail("conflict", "Upload SHA-256 mismatch")
	}
	if err := source.file.Sync(); err != nil {
		return ByteSource{}, err
	}
	source.descriptor.SHA256 = sum
	source.descriptor.Immutable = true
	return source.descriptor, nil
}
