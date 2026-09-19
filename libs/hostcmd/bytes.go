package hostcmd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"github.com/veypi/aic-pod/protocol/hosts"
)

type ByteSource struct {
	Ref       hosts.ResourceRef `json:"ref"`
	Size      int64             `json:"size"`
	MediaType string            `json:"media_type"`
	Seekable  bool              `json:"seekable"`
	Immutable bool              `json:"immutable"`
	SHA256    string            `json:"sha256,omitempty"`
	Version   string            `json:"version,omitempty"`
	Lifetime  string            `json:"lifetime"`
}
type byteSource struct {
	mu         sync.RWMutex
	owner      string
	descriptor ByteSource
	file       *os.File
	temp       string
	verify     func(context.Context) error
	closed     bool
}
type BytesConfig struct {
	TempDir        string
	MaxBytes       int64
	MaxSourceBytes int64
	MaxSources     int
}
type Bytes struct {
	mu            sync.Mutex
	cfg           BytesConfig
	epoch         string
	dir           string
	sources       map[string]*byteSource
	used          int64
	pending       int
	pendingOwners map[string]int
	closingOwners map[string]bool
	closed        bool
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
	epoch, err := hosts.NewID("bytes_")
	if err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	return &Bytes{cfg: cfg, dir: dir, epoch: epoch, sources: map[string]*byteSource{}, pendingOwners: map[string]int{}, closingOwners: map[string]bool{}}, nil
}
func (b *Bytes) reservation(owner string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed || b.closingOwners[owner] {
		return hosts.Fail("expired", "Byte store closed")
	}
	if len(b.sources)+b.pending >= b.cfg.MaxSources {
		return hosts.Fail("overloaded", "Byte source quota reached")
	}
	b.pending++
	b.pendingOwners[owner]++
	return nil
}
func (b *Bytes) finishReservation(owner string) {
	b.pending--
	b.pendingOwners[owner]--
	if b.pendingOwners[owner] == 0 {
		delete(b.pendingOwners, owner)
		delete(b.closingOwners, owner)
	}
}
func (b *Bytes) reserveBytes(n int64) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return hosts.Fail("expired", "Byte store closed")
	}
	if n < 0 || b.used > b.cfg.MaxBytes-n {
		return hosts.Fail("overloaded", "Byte storage quota reached")
	}
	b.used += n
	return nil
}

// Upload stages raw bytes on disk and exposes a source only after size and hash
// verification. A caller must already be authenticated and bound to owner.
func (b *Bytes) Upload(ctx context.Context, owner string, reader io.Reader, size *int64, sha string, mediaType string) (ByteSource, error) {
	if !hosts.ValidID(owner) || reader == nil {
		return ByteSource{}, hosts.Fail("invalid_argument", "Invalid upload owner or body")
	}
	if size != nil && (*size < 0 || *size > b.cfg.MaxSourceBytes) {
		return ByteSource{}, hosts.Fail("overloaded", "Upload size exceeds quota")
	}
	if sha != "" {
		if raw, err := hex.DecodeString(sha); err != nil || len(raw) != 32 {
			return ByteSource{}, hosts.Fail("invalid_argument", "Invalid SHA-256")
		}
		sha = strings.ToLower(sha)
	}
	if err := b.reservation(owner); err != nil {
		return ByteSource{}, err
	}
	var charged int64
	success := false
	defer func() {
		b.mu.Lock()
		b.finishReservation(owner)
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
			if charged+int64(n) > b.cfg.MaxSourceBytes || (size != nil && charged+int64(n) > *size) {
				return ByteSource{}, hosts.Fail("invalid_argument", "Upload exceeded declared size or quota")
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
		return ByteSource{}, hosts.Fail("invalid_argument", "Upload length or SHA-256 mismatch")
	}
	if err = file.Sync(); err != nil {
		return ByteSource{}, err
	}
	id, err := hosts.NewID("src_")
	if err != nil {
		return ByteSource{}, err
	}
	if mediaType == "" {
		mediaType = "application/octet-stream"
	}
	descriptor := ByteSource{Ref: hosts.ResourceRef{ID: id, Epoch: b.epoch, Kind: "bytes"}, Size: charged, MediaType: mediaType, Seekable: true, Immutable: true, SHA256: sum, Lifetime: "session"}
	b.mu.Lock()
	if b.closed || b.closingOwners[owner] {
		b.mu.Unlock()
		return ByteSource{}, hosts.Fail("expired", "Byte store closed")
	}
	b.sources[id] = &byteSource{owner: owner, descriptor: descriptor, file: file, temp: file.Name()}
	success = true
	b.mu.Unlock()
	return descriptor, nil
}

// AddFile transfers ownership of file, including on failure. verify rechecks
// both policy and source identity before/during/after a range read.
func (b *Bytes) AddFile(owner string, file *os.File, size int64, version, mediaType string, verify func(context.Context) error) (ByteSource, error) {
	if file == nil {
		return ByteSource{}, hosts.Fail("invalid_argument", "Missing source file")
	}
	success := false
	defer func() {
		if !success {
			file.Close()
		}
	}()
	if !hosts.ValidID(owner) || size < 0 || size > hosts.MaxSafeInteger || verify == nil {
		return ByteSource{}, hosts.Fail("invalid_argument", "Invalid source")
	}
	if err := b.reservation(owner); err != nil {
		return ByteSource{}, err
	}
	defer func() { b.mu.Lock(); b.finishReservation(owner); b.mu.Unlock() }()
	id, err := hosts.NewID("src_")
	if err != nil {
		return ByteSource{}, err
	}
	if mediaType == "" {
		mediaType = "application/octet-stream"
	}
	descriptor := ByteSource{Ref: hosts.ResourceRef{ID: id, Epoch: b.epoch, Kind: "bytes"}, Size: size, Version: version, MediaType: mediaType, Seekable: true, Lifetime: "session"}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed || b.closingOwners[owner] {
		return ByteSource{}, hosts.Fail("expired", "Byte store closed")
	}
	b.sources[id] = &byteSource{owner: owner, descriptor: descriptor, file: file, verify: verify}
	success = true
	return descriptor, nil
}
func (b *Bytes) source(owner string, ref hosts.ResourceRef) (*byteSource, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.sources[ref.ID]
	if b.closed || ref.Kind != "bytes" || ref.Epoch != b.epoch || s == nil || s.owner != owner {
		return nil, hosts.Fail("expired", "Byte source is unavailable in this session")
	}
	return s, nil
}

// SetVerifier attaches current policy checks to a staged command result.
// Transport/session authorization is still checked by the shared endpoint.
func (b *Bytes) SetVerifier(owner string, ref hosts.ResourceRef, verify func(context.Context) error) error {
	source, err := b.source(owner, ref)
	if err != nil {
		return err
	}
	source.mu.Lock()
	defer source.mu.Unlock()
	if source.closed {
		return hosts.Fail("expired", "Byte source closed")
	}
	source.verify = verify
	return nil
}
func (b *Bytes) Describe(owner string, ref hosts.ResourceRef) (ByteSource, error) {
	s, err := b.source(owner, ref)
	if err != nil {
		return ByteSource{}, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return ByteSource{}, hosts.Fail("expired", "Byte source closed")
	}
	return s.descriptor, nil
}
func (b *Bytes) Copy(ctx context.Context, owner string, ref hosts.ResourceRef, offset int64, length *int64, dst io.Writer) error {
	s, err := b.source(owner, ref)
	if err != nil {
		return err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return hosts.Fail("expired", "Byte source closed")
	}
	total := s.descriptor.Size
	if offset < 0 || offset > total || length != nil && (*length < 0 || *length > total-offset) {
		return hosts.Fail("invalid_argument", "Byte range outside source")
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
func (b *Bytes) Release(owner string, ref hosts.ResourceRef) error {
	b.mu.Lock()
	s := b.sources[ref.ID]
	if s == nil {
		b.mu.Unlock()
		return nil
	}
	if s.owner != owner || ref.Epoch != b.epoch || ref.Kind != "bytes" {
		b.mu.Unlock()
		return hosts.Fail("permission_denied", "Byte source belongs to another session")
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
func (b *Bytes) CloseSession(owner string) {
	b.mu.Lock()
	if b.pendingOwners[owner] > 0 {
		b.closingOwners[owner] = true
	}
	var refs []hosts.ResourceRef
	for _, s := range b.sources {
		if s.owner == owner {
			refs = append(refs, s.descriptor.Ref)
		}
	}
	b.mu.Unlock()
	for _, ref := range refs {
		_ = b.Release(owner, ref)
	}
}
func (b *Bytes) Close() error {
	b.mu.Lock()
	b.closed = true
	owners := map[string]bool{}
	for _, s := range b.sources {
		owners[s.owner] = true
	}
	b.mu.Unlock()
	for owner := range owners {
		b.CloseSession(owner)
	}
	return os.RemoveAll(b.dir)
}
