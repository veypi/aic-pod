//go:build darwin || linux

package hostfs

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/veypi/aic-pod/libs/hostcmd"
	fsp "github.com/veypi/aic-pod/protocol/fs"
	"github.com/veypi/aic-pod/protocol/hosts"
)

type fixture struct {
	rt      *hostcmd.Runtime
	fs      *FS
	store   *hostcmd.Bytes
	root    string
	caller  hostcmd.Caller
	session hostcmd.Session
	count   int
}

func setup(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{root: t.TempDir()}
	var err error
	f.store, err = hostcmd.NewBytes(hostcmd.BytesConfig{TempDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	f.fs, err = New(Config{Roots: []Root{{ID: "home", Name: "Home", Path: f.root, Default: true}}, Bytes: f.store, Check: func(_ context.Context, _ hostcmd.Call, path string, _ bool) error {
		if strings.Contains(filepath.Base(path), "denied") {
			return hosts.Fail("permission_denied", "Denied by local policy")
		}
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	f.rt, err = hostcmd.New(hostcmd.Config{Authorize: func(context.Context, hostcmd.Call) error { return nil }, OnSessionClose: f.store.CloseSession})
	if err != nil {
		t.Fatal(err)
	}
	if err = f.rt.Register(f.fs.Provider()); err != nil {
		t.Fatal(err)
	}
	f.caller = hostcmd.Caller{Subject: "owner", ConnectionID: "connection_1", ExpiresAt: time.Now().Add(time.Hour)}
	f.session, err = f.rt.Open(f.caller)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := f.rt.Shutdown(ctx); err != nil {
			t.Error(err)
		}
		f.fs.Close()
		f.store.Close()
	})
	return f
}
func loc(parts ...string) fsp.Path {
	if parts == nil {
		parts = []string{}
	}
	return fsp.Path{RootID: "home", Segments: parts}
}
func (f *fixture) call(t *testing.T, method string, args any) hosts.Operation {
	t.Helper()
	f.count++
	id := strings.Repeat("o", f.count)
	raw, _ := json.Marshal(args)
	op, err := f.rt.Invoke(context.Background(), f.caller, hosts.Invocation{SessionID: f.session.ID, RuntimeEpoch: f.session.RuntimeEpoch, OperationID: id, Command: "fs", Method: method, Args: raw})
	if err != nil {
		return hosts.Operation{Status: "failed", Error: hosts.AsFault(err)}
	}
	op, err = f.rt.Get(context.Background(), f.caller, f.session.ID, op.ID, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return op
}
func value[T any](t *testing.T, op hosts.Operation) T {
	t.Helper()
	if op.Status != "succeeded" {
		t.Fatalf("operation failed: %+v", op)
	}
	var v T
	if err := json.Unmarshal(op.Value, &v); err != nil {
		t.Fatal(err)
	}
	return v
}
func (f *fixture) upload(t *testing.T, data []byte) hosts.ResourceRef {
	t.Helper()
	size := int64(len(data))
	src, err := f.store.Upload(context.Background(), f.session.ID, bytes.NewReader(data), &size, "", "")
	if err != nil {
		t.Fatal(err)
	}
	return src.Ref
}
func TestRawContentConditionalSaveAndEmptyFiles(t *testing.T) {
	f := setup(t)
	data := append([]byte{0xef, 0xbb, 0xbf}, []byte(strings.Repeat("line\r\n", 1800)+"last line without newline")...)
	path := loc("note.txt")
	saved := value[fsp.Entry](t, f.call(t, "write", writeArgs{Path: path, Source: f.upload(t, data), Condition: fsp.Condition{Absent: true}}))
	disk, err := os.ReadFile(filepath.Join(f.root, "note.txt"))
	if err != nil || !bytes.Equal(disk, data) {
		t.Fatal("saved bytes differ")
	}
	current := value[fsp.Entry](t, f.call(t, "stat", pathArgs{Path: path}))
	if current.Version != saved.Version {
		t.Fatal("commit returned a stale version")
	}
	src := value[hostcmd.ByteSource](t, f.call(t, "read", pathArgs{Path: path}))
	var read bytes.Buffer
	if err = f.store.Copy(context.Background(), f.session.ID, src.Ref, 0, nil, &read); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, read.Bytes()) {
		t.Fatal("read changed line endings/BOM/long text")
	}
	next := f.upload(t, []byte{0, 255, 1, 0})
	value[fsp.Entry](t, f.call(t, "write", writeArgs{Path: path, Source: next, Condition: fsp.Condition{Version: saved.Version}}))
	stale := f.call(t, "write", writeArgs{Path: path, Source: next, Condition: fsp.Condition{Version: saved.Version}})
	if stale.Error == nil || stale.Error.Code != "version_conflict" {
		t.Fatalf("stale save accepted: %+v", stale)
	}
	if err = f.store.Copy(context.Background(), f.session.ID, src.Ref, 0, nil, &bytes.Buffer{}); err == nil {
		t.Fatal("old live source was not invalidated")
	}
	empty := value[fsp.Entry](t, f.call(t, "write", writeArgs{Path: loc("empty"), Source: f.upload(t, nil), Condition: fsp.Condition{Absent: true}}))
	if empty.Size == nil || *empty.Size != 0 {
		t.Fatal("empty file was lost")
	}
}
func TestPathsPolicyAndLinks(t *testing.T) {
	f := setup(t)
	for _, path := range []fsp.Path{loc("..", "outside"), loc("a/b"), loc(""), {RootID: "unknown", Segments: []string{}}, loc("denied.txt")} {
		if op := f.call(t, "stat", pathArgs{Path: path}); op.Status == "succeeded" {
			t.Fatalf("accepted invalid path: %v", path)
		}
	}
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(f.root, "link")); err != nil {
		t.Fatal(err)
	}
	if op := f.call(t, "read", pathArgs{Path: loc("link", "secret")}); op.Status == "succeeded" {
		t.Fatal("followed outside link")
	}
	if op := f.call(t, "write", writeArgs{Path: loc("denied.txt"), Source: f.upload(t, []byte("x")), Condition: fsp.Condition{Any: true}}); op.Error == nil || op.Error.Code != "permission_denied" {
		t.Fatalf("policy bypass: %+v", op)
	}
	if op := f.call(t, "write", writeArgs{Path: loc(), Source: f.upload(t, nil), Condition: fsp.Condition{Any: true}}); op.Status == "succeeded" {
		t.Fatal("root replace accepted")
	}
}
func TestDirectoryPagingMkdirAndNonRecursiveRemove(t *testing.T) {
	f := setup(t)
	for _, name := range []string{"a", "b", "c", ".hidden", "denied"} {
		if err := os.WriteFile(filepath.Join(f.root, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	type page struct {
		Entries []fsp.Entry `json:"entries"`
		Next    string      `json:"next_cursor"`
	}
	first := value[page](t, f.call(t, "list", listArgs{Path: loc(), Limit: 2}))
	if len(first.Entries) != 2 || first.Entries[0].Name != "a" || first.Next == "" {
		t.Fatalf("bad page: %+v", first)
	}
	second := value[page](t, f.call(t, "list", listArgs{Path: loc(), Limit: 2, Cursor: first.Next}))
	if len(second.Entries) != 1 || second.Entries[0].Name != "c" {
		t.Fatalf("bad next page: %+v", second)
	}
	dir := value[fsp.Entry](t, f.call(t, "mkdir", mkdirArgs{Path: loc("empty-dir")}))
	if dir.Kind != "directory" {
		t.Fatal("empty directory not created")
	}
	changed := f.call(t, "list", listArgs{Path: loc(), Limit: 2, Cursor: first.Next})
	if changed.Error == nil || changed.Error.Code != "cursor_invalid" {
		t.Fatalf("stale cursor accepted: %+v", changed)
	}
	if op := f.call(t, "remove", removeArgs{Path: loc("empty-dir"), IfVersion: dir.Version}); op.Status != "succeeded" {
		t.Fatalf("remove empty directory: %+v", op)
	}
	if err := os.Mkdir(filepath.Join(f.root, "full"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.root, "full", "x"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	full := value[fsp.Entry](t, f.call(t, "stat", pathArgs{Path: loc("full")}))
	if op := f.call(t, "remove", removeArgs{Path: loc("full"), IfVersion: full.Version}); op.Status == "succeeded" {
		t.Fatal("implicitly recursive remove")
	}
}

func TestMetadataSourcePolicyAllowsWriteConsumptionAndRevocation(t *testing.T) {
	f := setup(t)
	args, _ := json.Marshal(map[string]any{"path": loc()})
	verify, err := f.fs.ResultPolicy(hostcmd.Call{SessionID: f.session.ID, Command: "fs", Method: "list", Args: args})
	if err != nil {
		t.Fatal(err)
	}
	size := int64(2)
	source, err := f.store.Upload(context.Background(), f.session.ID, strings.NewReader("{}"), &size, "", "application/json")
	if err != nil {
		t.Fatal(err)
	}
	if err = f.store.SetVerifier(f.session.ID, source.Ref, verify); err != nil {
		t.Fatal(err)
	}
	op := f.call(t, "write", map[string]any{"path": loc("snapshot.json"), "source": source.Ref, "condition": map[string]any{"absent": true}})
	if op.Status != "succeeded" {
		t.Fatal("metadata source could not be written", op)
	}
	f.fs.cfg.Check = func(context.Context, hostcmd.Call, string, bool) error {
		return hosts.Fail("permission_denied", "changed policy")
	}
	if err = f.store.Copy(context.Background(), f.session.ID, source.Ref, 0, nil, &bytes.Buffer{}); err == nil || hosts.AsFault(err).Code != "permission_denied" {
		t.Fatal("snapshot policy not rechecked", err)
	}
}
