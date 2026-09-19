package hostcmd

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"hash"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/veypi/aic-pod/protocol/hosts"
)

// Transfers owns the file stream state for every transport. It stores bounded
// chunks, not a network goroutine/pipe, so a session can rebind without losing an
// admitted upload or replaying a filesystem mutation.
type TransferConfig struct {
	MaxUploadBytes   int64
	ProxyUploadBytes int64
	MaxStreams       int
	IdleTTL          time.Duration
}
type Transfers struct {
	mu      sync.Mutex
	cfg     TransferConfig
	bytes   *Bytes
	check   func(string, string) error
	streams map[string]*byteTransfer
	closed  bool
}
type byteTransfer struct {
	mu                    sync.Mutex
	session               string
	item                  hosts.StreamItem
	digest                [32]byte
	upload                bool
	file                  *os.File
	hash                  hash.Hash
	charged               int64
	reservation           bool
	source                ByteSource
	base                  int64
	offset, seq           int64
	final, sealed, closed bool
	last                  time.Time
	lastHash              [32]byte
	lastFrame             []byte
}
type TransferStatus struct {
	hosts.StreamItem
	Offset int64 `json:"offset"`
	Seq    int64 `json:"seq"`
	Final  bool  `json:"final"`
	Sealed bool  `json:"sealed"`
}

func NewTransfers(b *Bytes, check func(string, string) error, cfg TransferConfig) *Transfers {
	if cfg.MaxUploadBytes <= 0 {
		cfg.MaxUploadBytes = 512 << 20
	}
	cfg.MaxUploadBytes = min(cfg.MaxUploadBytes, b.cfg.MaxSourceBytes, b.cfg.MaxBytes, hosts.MaxSafeInteger)
	if cfg.ProxyUploadBytes <= 0 {
		cfg.ProxyUploadBytes = 64 << 20
	}
	if cfg.MaxStreams <= 0 {
		cfg.MaxStreams = 4
	}
	cfg.MaxStreams = min(cfg.MaxStreams, 128)
	if cfg.IdleTTL <= 0 {
		cfg.IdleTTL = 5 * time.Minute
	}
	return &Transfers{bytes: b, check: check, cfg: cfg, streams: map[string]*byteTransfer{}}
}
func (m *Transfers) Limits(proxy bool) map[string]any {
	size := m.cfg.MaxUploadBytes
	if proxy {
		size = min(size, m.cfg.ProxyUploadBytes)
	}
	return map[string]any{"control_bytes": hosts.MaxControlBytes, "data_bytes": hosts.MaxDataBytes, "chunk_bytes": hosts.ChunkBytes, "streams": m.cfg.MaxStreams, "upload_bytes": size}
}
func (t *byteTransfer) status() TransferStatus {
	return TransferStatus{StreamItem: t.item, Offset: t.offset, Seq: t.seq, Final: t.final, Sealed: t.sealed}
}
func (m *Transfers) get(conn, session, id string) (*byteTransfer, error) {
	if err := m.check(conn, session); err != nil {
		return nil, err
	}
	m.mu.Lock()
	t := m.streams[id]
	m.mu.Unlock()
	if t == nil || t.session != session {
		return nil, hosts.Fail("expired", "Byte stream is unavailable")
	}
	return t, nil
}
func (m *Transfers) Open(conn string, proxy, upload bool, raw json.RawMessage) (any, error) {
	var p struct {
		Session   string            `json:"session_id"`
		Stream    string            `json:"stream_id"`
		Size      *int64            `json:"size,omitempty"`
		MediaType string            `json:"media_type,omitempty"`
		Ref       hosts.ResourceRef `json:"ref,omitempty"`
		Offset    int64             `json:"offset,omitempty"`
		Length    *int64            `json:"length,omitempty"`
	}
	if err := hosts.Decode(raw, &p); err != nil {
		return nil, err
	}
	if !hosts.ValidID(p.Stream) {
		return nil, hosts.Fail("invalid_argument", "stream_id is required")
	}
	if err := m.check(conn, p.Session); err != nil {
		return nil, err
	}
	canonical, err := hosts.CanonicalArgs(raw)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(append([]byte{byte(0)}, canonical...))
	if upload {
		digest = sha256.Sum256(append([]byte{byte(1)}, canonical...))
	}
	var source ByteSource
	var size int64
	if upload {
		maxSize := m.cfg.MaxUploadBytes
		if proxy {
			maxSize = min(maxSize, m.cfg.ProxyUploadBytes)
		}
		if p.Size == nil || *p.Size < 0 || *p.Size > maxSize {
			return nil, hosts.Fail("overloaded", "Upload exceeds transport limit")
		}
		size = *p.Size
	} else {
		source, err = m.bytes.Describe(p.Session, p.Ref)
		if err != nil {
			return nil, err
		}
		if p.Offset < 0 || p.Offset > source.Size || p.Length != nil && (*p.Length < 0 || *p.Length > source.Size-p.Offset) {
			return nil, hosts.Fail("invalid_argument", "Invalid byte range")
		}
		size = source.Size - p.Offset
		if p.Length != nil {
			size = *p.Length
		}
		p.MediaType = source.MediaType
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, hosts.Fail("expired", "Byte streams closed")
	}
	if old := m.streams[p.Stream]; old != nil {
		old.mu.Lock()
		defer old.mu.Unlock()
		if old.session != p.Session || old.digest != digest {
			return nil, hosts.Fail("operation_conflict", "Stream ID already has different parameters")
		}
		if old.closed {
			return nil, hosts.Fail("expired", "Byte stream closed")
		}
		old.last = time.Now()
		return old.item, nil
	}
	active := 0
	for _, t := range m.streams {
		if t.session == p.Session {
			active++
		}
	}
	if active >= m.cfg.MaxStreams || len(m.streams) >= 128 {
		return nil, hosts.Fail("overloaded", "Byte stream quota reached")
	}
	itemID, err := hosts.NewID("item_")
	if err != nil {
		return nil, err
	}
	if p.MediaType == "" {
		p.MediaType = "application/octet-stream"
	}
	t := &byteTransfer{session: p.Session, item: hosts.StreamItem{StreamID: p.Stream, ItemID: itemID, Size: size, MediaType: p.MediaType}, digest: digest, upload: upload, last: time.Now(), source: source, base: p.Offset}
	if upload {
		if err = m.bytes.reservation(p.Session); err != nil {
			return nil, err
		}
		t.reservation = true
		t.file, err = os.CreateTemp(m.bytes.dir, "stream-")
		if err != nil {
			m.bytes.mu.Lock()
			m.bytes.finishReservation(p.Session)
			m.bytes.mu.Unlock()
			return nil, err
		}
		t.hash = sha256.New()
	}
	m.streams[p.Stream] = t
	return t.item, nil
}
func (m *Transfers) Write(conn string, h hosts.Frame, data []byte, proxy bool) (any, error) {
	t, err := m.get(conn, h.SessionID, h.StreamID)
	if err != nil {
		return nil, err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if err = m.check(conn, t.session); err != nil {
		return nil, err
	}
	if proxy && t.item.Size > m.cfg.ProxyUploadBytes {
		return nil, hosts.Fail("overloaded", "Upload exceeds proxy budget; resume using RTC")
	}
	if t.closed || !t.upload || h.ItemID != t.item.ItemID || len(data) > hosts.ChunkBytes {
		return nil, hosts.Fail("invalid_argument", "Invalid upload frame")
	}
	// request_id is a transmission identity; retries of one chunk can use a new ID.
	stable := h
	stable.RequestID = ""
	encoded, _ := hosts.EncodeFrame(stable, data)
	digest := sha256.Sum256(encoded)
	if h.Seq == t.seq-1 && t.lastHash == digest {
		t.last = time.Now()
		return t.status(), nil
	}
	if t.final || h.Seq != t.seq || h.Offset != t.offset || t.offset+int64(len(data)) > t.item.Size || (!h.Final && len(data) == 0) || (h.Final && t.offset+int64(len(data)) != t.item.Size) {
		return nil, hosts.Fail("invalid_argument", "Upload sequence, offset or length mismatch")
	}
	if err = m.bytes.reserveBytes(int64(len(data))); err != nil {
		return nil, err
	}
	t.charged += int64(len(data))
	if len(data) > 0 {
		n, writeErr := t.file.Write(data)
		err = writeErr
		if err == nil && n != len(data) {
			err = io.ErrShortWrite
		}
		if err != nil {
			m.dispose(t)
			return nil, err
		}
		t.hash.Write(data)
	}
	t.offset += int64(len(data))
	t.seq++
	t.final = h.Final
	t.lastHash = digest
	t.last = time.Now()
	return t.status(), nil
}
func (m *Transfers) Pull(ctx context.Context, conn string, raw json.RawMessage, request string) ([]byte, error) {
	var p struct {
		Session string `json:"session_id"`
		Stream  string `json:"stream_id"`
		Seq     int64  `json:"seq"`
		Offset  int64  `json:"offset"`
		Bytes   int64  `json:"bytes"`
	}
	if err := hosts.Decode(raw, &p); err != nil {
		return nil, err
	}
	t, err := m.get(conn, p.Session, p.Stream)
	if err != nil {
		return nil, err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if err = m.check(conn, t.session); err != nil {
		return nil, err
	}
	if t.closed || t.upload || p.Bytes < 0 || p.Bytes > hosts.ChunkBytes {
		return nil, hosts.Fail("invalid_argument", "Invalid download pull")
	}
	if p.Seq == t.seq-1 && len(t.lastFrame) > 0 {
		h, data, e := hosts.DecodeFrame(t.lastFrame)
		if e != nil {
			return nil, e
		}
		if h.Offset != p.Offset || int64(len(data)) != p.Bytes {
			return nil, hosts.Fail("invalid_argument", "Conflicting pull replay")
		}
		zero := int64(0)
		if e = m.bytes.Copy(ctx, t.session, t.source.Ref, t.base, &zero, &bytes.Buffer{}); e != nil {
			return nil, e
		}
		h.RequestID = request
		t.last = time.Now()
		return hosts.EncodeFrame(h, data)
	}
	if t.final || p.Seq != t.seq || p.Offset != t.offset || p.Bytes > t.item.Size-t.offset || (p.Bytes == 0 && t.offset < t.item.Size) {
		return nil, hosts.Fail("invalid_argument", "Download sequence or range mismatch")
	}
	var out bytes.Buffer
	if err = m.bytes.Copy(ctx, t.session, t.source.Ref, t.base+t.offset, &p.Bytes, &out); err != nil {
		return nil, err
	}
	h := hosts.Frame{RequestID: request, SessionID: t.session, StreamID: t.item.StreamID, ItemID: t.item.ItemID, Seq: t.seq, Offset: t.offset, Final: t.offset+p.Bytes == t.item.Size}
	frame, err := hosts.EncodeFrame(h, out.Bytes())
	if err != nil {
		return nil, err
	}
	t.seq++
	t.offset += p.Bytes
	t.final = h.Final
	t.last = time.Now()
	t.lastFrame = frame
	return frame, nil
}
func (m *Transfers) Status(conn, session, stream string) (any, error) {
	t, err := m.get(conn, session, stream)
	if err != nil {
		return nil, err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return nil, hosts.Fail("expired", "Byte stream closed")
	}
	t.last = time.Now()
	return t.status(), nil
}
func (m *Transfers) Seal(conn string, raw json.RawMessage) (any, error) {
	var p struct {
		Session string `json:"session_id"`
		Stream  string `json:"stream_id"`
		Size    int64  `json:"actual_size"`
		SHA     string `json:"sha256,omitempty"`
	}
	if err := hosts.Decode(raw, &p); err != nil {
		return nil, err
	}
	t, err := m.get(conn, p.Session, p.Stream)
	if err != nil {
		return nil, err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if err = m.check(conn, t.session); err != nil {
		return nil, err
	}
	if !t.upload || !t.final || t.closed || p.Size != t.offset {
		return nil, hosts.Fail("invalid_argument", "Upload is incomplete")
	}
	sum := hex.EncodeToString(t.hash.Sum(nil))
	if p.SHA != "" && !strings.EqualFold(sum, p.SHA) {
		return nil, hosts.Fail("invalid_argument", "Upload SHA-256 mismatch")
	}
	if t.sealed {
		return m.bytes.Describe(t.session, t.source.Ref)
	}
	if err = t.file.Sync(); err != nil {
		return nil, err
	}
	id, err := hosts.NewID("src_")
	if err != nil {
		return nil, err
	}
	b := m.bytes
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed || b.closingOwners[t.session] {
		return nil, hosts.Fail("expired", "Byte store closed")
	}
	source := ByteSource{Ref: hosts.ResourceRef{ID: id, Epoch: b.epoch, Kind: "bytes"}, Size: t.offset, MediaType: t.item.MediaType, Seekable: true, Immutable: true, SHA256: sum, Lifetime: "session"}
	b.sources[id] = &byteSource{owner: t.session, descriptor: source, file: t.file, temp: t.file.Name()}
	b.finishReservation(t.session)
	t.reservation = false
	t.sealed = true
	t.source = source
	t.last = time.Now()
	return source, nil
}
func (m *Transfers) Cancel(conn, session, stream string) error {
	if err := m.check(conn, session); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	t := m.streams[stream]
	if t == nil {
		return nil
	}
	if t.session != session {
		return hosts.Fail("permission_denied", "Stream belongs to another session")
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	m.dispose(t)
	delete(m.streams, stream)
	return nil
}

// dispose is called with t.mu held; a sealed source has its own resource lifetime.
func (m *Transfers) dispose(t *byteTransfer) {
	if t.closed {
		return
	}
	t.closed = true
	if t.upload && !t.sealed && t.file != nil {
		t.file.Close()
		os.Remove(t.file.Name())
	}
	if t.reservation {
		m.bytes.mu.Lock()
		m.bytes.used -= t.charged
		m.bytes.finishReservation(t.session)
		m.bytes.mu.Unlock()
		t.reservation = false
	}
}
func (m *Transfers) CloseSession(session string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, t := range m.streams {
		if t.session == session {
			t.mu.Lock()
			m.dispose(t)
			t.mu.Unlock()
			delete(m.streams, id)
		}
	}
}
func (m *Transfers) Reap() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, t := range m.streams {
		t.mu.Lock()
		if time.Since(t.last) > m.cfg.IdleTTL || t.closed {
			m.dispose(t)
			delete(m.streams, id)
		}
		t.mu.Unlock()
	}
}
func (m *Transfers) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	for id, t := range m.streams {
		t.mu.Lock()
		m.dispose(t)
		t.mu.Unlock()
		delete(m.streams, id)
	}
}
