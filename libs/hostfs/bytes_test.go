package hostfs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"testing"
)

func TestBytesRoundTripRangeAndOwnership(t *testing.T) {
	store, err := NewBytes(BytesConfig{TempDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	data := append([]byte{0, 255, 0, 128}, bytes.Repeat([]byte("line\r\n"), 20000)...)
	hash := sha256.Sum256(data)
	size := int64(len(data))
	src, err := store.Upload(context.Background(), "session_1", bytes.NewReader(data), &size, hex.EncodeToString(hash[:]), "application/octet-stream")
	if err != nil {
		t.Fatal(err)
	}
	var got bytes.Buffer
	if err = store.Copy(context.Background(), "session_1", src.Ref, 0, nil, &got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.Bytes(), data) {
		t.Fatal("raw bytes changed")
	}
	got.Reset()
	n := int64(11)
	if err = store.Copy(context.Background(), "session_1", src.Ref, 2, &n, &got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.Bytes(), data[2:13]) {
		t.Fatal("range mismatch")
	}
	if err = store.Copy(context.Background(), "another_session", src.Ref, 0, nil, io.Discard); err == nil {
		t.Fatal("cross-session source access")
	}
	if err = store.Release("another_session", src.Ref); err == nil {
		t.Fatal("cross-session release")
	}
	store.CloseOwner("session_1")
	if _, err = store.Describe("session_1", src.Ref); err == nil {
		t.Fatal("released source remained accessible")
	}
}
func TestIncompleteUploadNeverCreatesSource(t *testing.T) {
	store, err := NewBytes(BytesConfig{TempDir: t.TempDir(), MaxBytes: 8, MaxSourceBytes: 8})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	size := int64(4)
	for _, data := range [][]byte{[]byte("abc"), []byte("abcde")} {
		if _, err := store.Upload(context.Background(), "session_1", bytes.NewReader(data), &size, "", ""); err == nil {
			t.Fatal("bad length accepted")
		}
	}
	if _, err := store.Upload(context.Background(), "session_1", bytes.NewReader([]byte("abcdefghx")), nil, "", ""); err == nil {
		t.Fatal("quota bypass")
	}
	if store.used != 0 || len(store.sources) != 0 {
		t.Fatalf("failed upload leaked reservation: %d", store.used)
	}
	zero := int64(0)
	src, err := store.Upload(context.Background(), "session_1", bytes.NewReader(nil), &zero, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if src.Size != 0 {
		t.Fatal("empty content changed")
	}
	if err = store.Copy(context.Background(), "session_1", src.Ref, 0, &zero, io.Discard); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = store.Upload(ctx, "session_1", bytes.NewReader([]byte("x")), nil, "", ""); err == nil {
		t.Fatal("cancelled upload accepted")
	}
}

type readFunc func([]byte) (int, error)

func (f readFunc) Read(p []byte) (int, error) { return f(p) }

func TestSessionCloseDuringUploadDoesNotPublishSource(t *testing.T) {
	store, err := NewBytes(BytesConfig{TempDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	started, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := store.Upload(context.Background(), "session_1", readFunc(func(p []byte) (int, error) {
			close(started)
			<-release
			return copy(p, "pending"), io.EOF
		}), nil, "", "")
		done <- err
	}()
	<-started
	store.CloseOwner("session_1")
	close(release)
	if err := <-done; err == nil {
		t.Fatal("upload published after its session closed")
	}
	if store.used != 0 || store.pending != 0 || len(store.sources) != 0 || len(store.pendingOwners) != 0 || len(store.closingOwners) != 0 {
		t.Fatal("closed upload leaked storage or bookkeeping")
	}
}
