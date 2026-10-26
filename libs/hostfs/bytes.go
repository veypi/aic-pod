package hostfs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"github.com/veypi/aic-pod/protocol"
	"io"
	"os"
	"strings"
	"sync"
	"time"
)

// sourceTTL 是字节源的存活期：客户端没显式 source.release 的泄漏源只能靠它兜底回收。
// 30 分钟太长——页面刷新/断连丢一次释放就占住一个槽位半小时，几十次刷新即可把
// MaxSources 打满、连写操作一起拒掉（2026-10-26 实测）。消费者（预览/下载/流式读）
// 都是秒级到分钟级行为，5 分钟足够宽裕。
const sourceTTL = 5 * time.Minute

type ByteSource struct {
	Ref       protocol.FSResourceRef `json:"ref"`
	Size      int64                  `json:"size"`
	MediaType string                 `json:"media_type"`
	Seekable  bool                   `json:"seekable"`
	Immutable bool                   `json:"immutable"`
	SHA256    string                 `json:"sha256,omitempty"`
	Version   string                 `json:"version,omitempty"`
	Lifetime  string                 `json:"lifetime"`
}
type byteSource struct {
	mu         sync.RWMutex
	owner      string // Subject+Origin：授权与跨通道共享
	creator    string // Owner+ConnectionID：断连回收边界
	descriptor ByteSource
	file       *os.File
	temp       string
	verify     func(context.Context) error
	closed     bool
	expires    time.Time
	received   int64
}
type BytesConfig struct {
	TempDir        string
	MaxBytes       int64
	MaxSourceBytes int64
	MaxSources     int
}
type Bytes struct {
	mu              sync.Mutex
	cfg             BytesConfig
	epoch           string
	dir             string
	sources         map[string]*byteSource
	used            int64
	pending         int
	pendingCreators map[string]int
	closingCreators map[string]bool
	closed          bool
}

func NewBytes(cfg BytesConfig) (*Bytes, error) {
	if cfg.MaxBytes <= 0 {
		cfg.MaxBytes = 2 << 30
	}
	if cfg.MaxSourceBytes <= 0 {
		cfg.MaxSourceBytes = 512 << 20
	}
	if cfg.MaxSources <= 0 {
		cfg.MaxSources = 128
	}
	dir, err := os.MkdirTemp(cfg.TempDir, "aic-host-bytes-")
	if err != nil {
		return nil, err
	}
	epoch := protocol.NewID("bytes_")
	return &Bytes{cfg: cfg, dir: dir, epoch: epoch, sources: map[string]*byteSource{}, pendingCreators: map[string]int{}, closingCreators: map[string]bool{}}, nil
}
func (b *Bytes) reservation(creator string) error {
	b.Reap()
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed || b.closingCreators[creator] {
		return protocol.FSFail("expired", "Byte store closed")
	}
	if len(b.sources)+b.pending >= b.cfg.MaxSources {
		return protocol.FSFail("overloaded", "Byte source quota reached")
	}
	b.pending++
	b.pendingCreators[creator]++
	return nil
}
func (b *Bytes) finishReservation(creator string) {
	b.pending--
	b.pendingCreators[creator]--
	if b.pendingCreators[creator] == 0 {
		delete(b.pendingCreators, creator)
		delete(b.closingCreators, creator)
	}
}
func (b *Bytes) reserveBytes(n int64) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return protocol.FSFail("expired", "Byte store closed")
	}
	if n < 0 || b.used > b.cfg.MaxBytes-n {
		return protocol.FSFail("overloaded", "Byte storage quota reached")
	}
	b.used += n
	return nil
}

// Upload stages raw bytes with owner as both authorization and lifecycle key.
func (b *Bytes) Upload(ctx context.Context, owner string, reader io.Reader, size *int64, sha string, mediaType string) (ByteSource, error) {
	return b.UploadFor(ctx, owner, owner, reader, size, sha, mediaType)
}

// UploadFor stages raw bytes on disk and exposes a source only after size and
// hash verification. owner authorizes later access; creator bounds disconnect
// cleanup. A caller must already be authenticated and bound to owner.
func (b *Bytes) UploadFor(ctx context.Context, owner, creator string, reader io.Reader, size *int64, sha string, mediaType string) (ByteSource, error) {
	limit := b.sourceLimit()
	if creator == "" {
		creator = owner
	}
	if !protocol.ValidID(owner) || reader == nil {
		return ByteSource{}, protocol.FSFail("invalid_argument", "Invalid upload owner or body")
	}
	if size != nil && (*size < 0 || *size > limit) {
		return ByteSource{}, protocol.FSFail("overloaded", "Upload size exceeds quota")
	}
	if sha != "" {
		if raw, err := hex.DecodeString(sha); err != nil || len(raw) != 32 {
			return ByteSource{}, protocol.FSFail("invalid_argument", "Invalid SHA-256")
		}
		sha = strings.ToLower(sha)
	}
	if err := b.reservation(creator); err != nil {
		return ByteSource{}, err
	}
	var charged int64
	success := false
	defer func() {
		b.mu.Lock()
		b.finishReservation(creator)
		if !success {
			b.used -= charged
		}
		b.mu.Unlock()
	}()
	file, err := os.CreateTemp(b.dir, "upload-")
	if err != nil {
		return ByteSource{}, err
	}
	defer func() {
		if !success {
			file.Close()
			os.Remove(file.Name())
		}
	}()
	hash := sha256.New()
	buf := make([]byte, 64<<10)
	for {
		if err = ctx.Err(); err != nil {
			return ByteSource{}, err
		}
		n, readErr := reader.Read(buf)
		if n > 0 {
			if charged+int64(n) > limit || (size != nil && charged+int64(n) > *size) {
				return ByteSource{}, protocol.FSFail("invalid_argument", "Upload exceeded declared size or quota")
			}
			if err = b.reserveBytes(int64(n)); err != nil {
				return ByteSource{}, err
			}
			charged += int64(n)
			if _, err = file.Write(buf[:n]); err != nil {
				return ByteSource{}, err
			}
			hash.Write(buf[:n])
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return ByteSource{}, readErr
		}
		if n == 0 {
			return ByteSource{}, io.ErrNoProgress
		}
	}
	if err = ctx.Err(); err != nil {
		return ByteSource{}, err
	}
	sum := hex.EncodeToString(hash.Sum(nil))
	if (size != nil && charged != *size) || (sha != "" && sum != sha) {
		return ByteSource{}, protocol.FSFail("invalid_argument", "Upload length or SHA-256 mismatch")
	}
	if err = file.Sync(); err != nil {
		return ByteSource{}, err
	}
	id := protocol.NewID("src_")
	if mediaType == "" {
		mediaType = "application/octet-stream"
	}
	descriptor := ByteSource{Ref: protocol.FSResourceRef{ID: id, Epoch: b.epoch, Kind: "bytes"}, Size: charged, MediaType: mediaType, Seekable: true, Immutable: true, SHA256: sum, Lifetime: "fs_source"}
	b.mu.Lock()
	if b.closed || b.closingCreators[creator] {
		b.mu.Unlock()
		return ByteSource{}, protocol.FSFail("expired", "Byte store closed")
	}
	b.sources[id] = &byteSource{expires: time.Now().Add(sourceTTL), owner: owner, creator: creator, descriptor: descriptor, file: file, temp: file.Name()}
	success = true
	b.mu.Unlock()
	return descriptor, nil
}

// AddFile transfers ownership of file with owner as both authorization and lifecycle key.
func (b *Bytes) AddFile(owner string, file *os.File, size int64, version, mediaType string, verify func(context.Context) error) (ByteSource, error) {
	return b.AddFileFor(owner, owner, file, size, version, mediaType, verify)
}

// AddFileFor transfers ownership of file, including on failure. verify rechecks
// both policy and source identity before/during/after a range read.
func (b *Bytes) AddFileFor(owner, creator string, file *os.File, size int64, version, mediaType string, verify func(context.Context) error) (ByteSource, error) {
	if creator == "" {
		creator = owner
	}
	if file == nil {
		return ByteSource{}, protocol.FSFail("invalid_argument", "Missing source file")
	}
	success := false
	defer func() {
		if !success {
			file.Close()
		}
	}()
	if !protocol.ValidID(owner) || size < 0 || size > protocol.FSMaxSafeInteger || verify == nil {
		return ByteSource{}, protocol.FSFail("invalid_argument", "Invalid source")
	}
	if err := b.reservation(creator); err != nil {
		return ByteSource{}, err
	}
	defer func() { b.mu.Lock(); b.finishReservation(creator); b.mu.Unlock() }()
	id := protocol.NewID("src_")
	if mediaType == "" {
		mediaType = "application/octet-stream"
	}
	descriptor := ByteSource{Ref: protocol.FSResourceRef{ID: id, Epoch: b.epoch, Kind: "bytes"}, Size: size, Version: version, MediaType: mediaType, Seekable: true, Lifetime: "fs_source"}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed || b.closingCreators[creator] {
		return ByteSource{}, protocol.FSFail("expired", "Byte store closed")
	}
	b.sources[id] = &byteSource{expires: time.Now().Add(sourceTTL), owner: owner, creator: creator, descriptor: descriptor, file: file, verify: verify}
	success = true
	return descriptor, nil
}
func (b *Bytes) source(owner string, ref protocol.FSResourceRef) (*byteSource, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.sources[ref.ID]
	if b.closed || ref.Kind != "bytes" || ref.Epoch != b.epoch || s == nil || s.owner != owner || time.Now().After(s.expires) {
		return nil, protocol.FSFail("expired", "Byte source is unavailable for this owner")
	}
	return s, nil
}
func (b *Bytes) Describe(owner string, ref protocol.FSResourceRef) (ByteSource, error) {
	s, err := b.source(owner, ref)
	if err != nil {
		return ByteSource{}, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return ByteSource{}, protocol.FSFail("expired", "Byte source closed")
	}
	return s.descriptor, nil
}
func (b *Bytes) Copy(ctx context.Context, owner string, ref protocol.FSResourceRef, offset int64, length *int64, dst io.Writer) error {
	s, err := b.source(owner, ref)
	if err != nil {
		return err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return protocol.FSFail("expired", "Byte source closed")
	}
	total := s.descriptor.Size
	if offset < 0 || offset > total || length != nil && (*length < 0 || *length > total-offset) {
		return protocol.FSFail("invalid_argument", "Byte range outside source")
	}
	count := total - offset
	if length != nil {
		count = *length
	}
	verify := func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if s.verify != nil {
			return s.verify(ctx)
		}
		return nil
	}
	if err := verify(); err != nil {
		return err
	}
	reader := io.NewSectionReader(s.file, offset, count)
	buf := make([]byte, 64<<10)
	for count > 0 {
		if err := verify(); err != nil {
			return err
		}
		n, err := reader.Read(buf[:min(int64(len(buf)), count)])
		if n > 0 {
			written, writeErr := dst.Write(buf[:n])
			if writeErr != nil {
				return writeErr
			}
			if written != n {
				return io.ErrShortWrite
			}
			count -= int64(n)
		}
		if err != nil {
			if err == io.EOF && count == 0 {
				break
			}
			return fmt.Errorf("source stream incomplete: %w", err)
		}
		if n == 0 {
			return io.ErrNoProgress
		}
	}
	return verify()
}
func (b *Bytes) Release(owner string, ref protocol.FSResourceRef) error {
	b.mu.Lock()
	s := b.sources[ref.ID]
	if s == nil {
		b.mu.Unlock()
		return nil
	}
	if s.owner != owner || ref.Epoch != b.epoch || ref.Kind != "bytes" {
		b.mu.Unlock()
		return protocol.FSFail("permission_denied", "Byte source belongs to another owner")
	}
	delete(b.sources, ref.ID)
	b.mu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	s.file.Close()
	if s.temp != "" {
		os.Remove(s.temp)
		b.mu.Lock()
		b.used -= s.descriptor.Size
		b.mu.Unlock()
	}
	return nil
}

// CloseCreator releases only sources created by one connection and prevents an
// in-flight creation from publishing after that connection closes.
func (b *Bytes) CloseCreator(creator string) {
	b.mu.Lock()
	if b.pendingCreators[creator] > 0 {
		b.closingCreators[creator] = true
	}
	type ownedRef struct {
		owner string
		ref   protocol.FSResourceRef
	}
	var refs []ownedRef
	for _, s := range b.sources {
		if s.creator == creator {
			refs = append(refs, ownedRef{owner: s.owner, ref: s.descriptor.Ref})
		}
	}
	b.mu.Unlock()
	for _, ref := range refs {
		_ = b.Release(ref.owner, ref.ref)
	}
}
func (b *Bytes) Close() error {
	b.mu.Lock()
	b.closed = true
	type ownedRef struct {
		owner string
		ref   protocol.FSResourceRef
	}
	var refs []ownedRef
	for _, s := range b.sources {
		refs = append(refs, ownedRef{owner: s.owner, ref: s.descriptor.Ref})
	}
	b.mu.Unlock()
	for _, ref := range refs {
		_ = b.Release(ref.owner, ref.ref)
	}
	return os.RemoveAll(b.dir)
}

func (b *Bytes) Reap() {
	b.mu.Lock()
	type key struct {
		owner string
		ref   protocol.FSResourceRef
	}
	expired := []key{}
	for _, s := range b.sources {
		if time.Now().After(s.expires) {
			expired = append(expired, key{s.owner, s.descriptor.Ref})
		}
	}
	b.mu.Unlock()
	for _, s := range expired {
		_ = b.Release(s.owner, s.ref)
	}
}

func (b *Bytes) sourceLimit() int64 { b.mu.Lock(); defer b.mu.Unlock(); return b.cfg.MaxSourceBytes }
