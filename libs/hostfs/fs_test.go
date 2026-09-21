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

	tool "github.com/veypi/aic-pod/libs/hosts_tool"
	fsp "github.com/veypi/aic-pod/protocol/fs"
	hosts "github.com/veypi/aic-pod/protocol/fs"
)

type fixture struct {
	fs     *FS
	store  *Bytes
	root   string
	caller tool.Caller
	owner  string
	count  int
}

func setup(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{root: t.TempDir()}
	var err error
	f.store, err = NewBytes(BytesConfig{TempDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	f.fs, err = New(Config{Roots: []Root{{ID: "home", Name: "Home", Path: f.root, Default: true}}, Bytes: f.store, Check: func(_ context.Context, _ Call, path string, _ bool) error {
		if strings.Contains(filepath.Base(path), "denied") {
			return hosts.Fail("permission_denied", "Denied by local policy")
		}
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	f.caller = tool.Caller{Subject: "owner", ConnectionID: "test", Level: 9, ExpiresAt: time.Now().Add(time.Hour)}
	f.owner = Owner(f.caller)
	t.Cleanup(func() {
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
func (f *fixture) call(t *testing.T, method string, args any) outcome {
	t.Helper()
	raw, _ := json.Marshal(args)
	result, err := f.fs.Run(context.Background(), Call{Caller: f.caller, Owner: f.owner, Command: "fs", Method: method, Args: raw})
	if err != nil {
		fault := hosts.AsFault(err)
		status := "failed"
		if fault.Code == "cancelled" {
			status = "cancelled"
		}
		return outcome{Status: status, Error: fault}
	}
	value, _ := json.Marshal(result)
	return outcome{Status: "succeeded", Value: value}
}

type outcome struct {
	Status string
	Error  *hosts.Fault
	Value  json.RawMessage
}

func value[T any](t *testing.T, op outcome) T {
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
	src, err := f.store.Upload(context.Background(), f.owner, bytes.NewReader(data), &size, "", "")
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
	src := value[ByteSource](t, f.call(t, "read", pathArgs{Path: path}))
	var read bytes.Buffer
	if err = f.store.Copy(context.Background(), f.owner, src.Ref, 0, nil, &read); err != nil {
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
	if err = f.store.Copy(context.Background(), f.owner, src.Ref, 0, nil, &bytes.Buffer{}); err == nil {
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
	verify, err := f.fs.ResultPolicy(Call{Owner: f.owner, Command: "fs", Method: "list", Args: args})
	if err != nil {
		t.Fatal(err)
	}
	size := int64(2)
	source, err := f.store.Upload(context.Background(), f.owner, strings.NewReader("{}"), &size, "", "application/json")
	if err != nil {
		t.Fatal(err)
	}
	if err = f.store.SetVerifier(f.owner, source.Ref, verify); err != nil {
		t.Fatal(err)
	}
	op := f.call(t, "write", map[string]any{"path": loc("snapshot.json"), "source": source.Ref, "condition": map[string]any{"absent": true}})
	if op.Status != "succeeded" {
		t.Fatal("metadata source could not be written", op)
	}
	f.fs.cfg.Check = func(context.Context, Call, string, bool) error {
		return hosts.Fail("permission_denied", "changed policy")
	}
	if err = f.store.Copy(context.Background(), f.owner, source.Ref, 0, nil, &bytes.Buffer{}); err == nil || hosts.AsFault(err).Code != "permission_denied" {
		t.Fatal("snapshot policy not rechecked", err)
	}
}
