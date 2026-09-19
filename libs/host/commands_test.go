//go:build darwin || linux

package host

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/veypi/aic-pod/cfg"
	"github.com/veypi/aic-pod/libs/hostcmd"
	fsp "github.com/veypi/aic-pod/protocol/fs"
	wire "github.com/veypi/aic-pod/protocol/hosts"
)

func wireValue[T any](t *testing.T, value any) T {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var out T
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// This exercises real credential admission, the JSON wire boundary, retained
// operations, raw bytes and device file policy shared by direct and proxied management.
// It deliberately does not pretend to exercise RTC/DTLS or the frontend.
func TestCommandServiceAuthenticatedFileLifecycle(t *testing.T) {
	original := cfg.AuthSnapshot()
	t.Cleanup(func() { cfg.SetAuth(original) })
	cfg.SetAuth(cfg.AuthCfg{})
	root := t.TempDir()
	c := New(Options{Key: "host_1.2.test-secret.owner_1", WorkDir: root})
	s, err := c.NewCommandService(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := s.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	ctx := context.Background()
	key, _ := wire.DirectKey("test-secret", "host_1")
	fingerprint := "sha-256 " + strings.TrimSuffix(strings.Repeat("AB:", 32), ":")
	admit := func(pc string) string {
		t.Helper()
		token, err := wire.SignTicket(key, wire.Ticket{HostID: "host_1", UserID: "owner_1", CredentialVersion: 2, PCID: pc, Fingerprint: fingerprint}, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		a, err := s.Access.Admit(token, pc, fingerprint)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Access.Bind(a.Caller.ConnectionID, pc, a.DataToken); err != nil {
			t.Fatal(err)
		}
		return a.Caller.ConnectionID
	}
	connection := "unauthenticated"
	request := func(method string, params any) wire.Response {
		t.Helper()
		p, err := json.Marshal(params)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(wire.Request{V: 1, Type: "request", ID: "req_1", Method: method, Params: p})
		if err != nil {
			t.Fatal(err)
		}
		return s.Handle(ctx, connection, raw)
	}
	if r := request("session.open", map[string]any{}); r.OK || r.Error.Code != "unauthorized" {
		t.Fatalf("unauthenticated access: %+v", r)
	}
	connection = admit("pc_1")
	r := request("session.open", map[string]any{})
	if !r.OK {
		t.Fatal(r.Error)
	}
	session := wireValue[hostcmd.Session](t, r.Result)
	if r := request("session.open", map[string]any{"user_id": "another_owner"}); r.OK {
		t.Fatal("frontend supplied identity accepted")
	}
	invoke := func(id, method string, args any) wire.Response {
		p, err := json.Marshal(args)
		if err != nil {
			t.Fatal(err)
		}
		return request("invoke", wire.Invocation{SessionID: session.ID, RuntimeEpoch: session.RuntimeEpoch, OperationID: id, Command: "fs", Method: method, Args: p})
	}
	get := func(id string) wire.Response {
		return request("operation.get", map[string]any{"session_id": session.ID, "operation_id": id, "wait_ms": 1000})
	}
	complete := func(id, method string, args any) wire.Operation {
		t.Helper()
		if r := invoke(id, method, args); !r.OK {
			t.Fatal(r.Error)
		}
		r := get(id)
		if !r.OK {
			t.Fatal(r.Error)
		}
		op := wireValue[wire.Operation](t, r.Result)
		if op.Status != "succeeded" {
			t.Fatalf("%s: %+v", method, op)
		}
		return op
	}
	roots := wireValue[struct {
		Roots []struct {
			ID         string `json:"id"`
			NativePath string `json:"native_path"`
		} `json:"roots"`
	}](t, json.RawMessage(complete("op_roots", "roots", map[string]any{}).Value))
	if len(roots.Roots) != 1 {
		t.Fatalf("roots: %+v", roots)
	}
	if roots.Roots[0].NativePath != "/" {
		t.Fatalf("filesystem narrowed to workspace: %+v", roots)
	}
	home := wireValue[fsp.Path](t, json.RawMessage(complete("op_home", "home", map[string]any{}).Value))
	if "/"+strings.Join(home.Segments, "/") != filepath.ToSlash(root) {
		t.Fatalf("initial directory: %+v", home)
	}
	// Exercise the authenticated protocol on a separate directory outside WorkDir.
	outside := t.TempDir()
	location := func(native string) fsp.Path {
		return fsp.Path{RootID: roots.Roots[0].ID, Segments: strings.Split(strings.TrimPrefix(filepath.ToSlash(native), "/"), "/")}
	}
	path := location(filepath.Join(outside, "原文.txt"))
	data := append([]byte{0xef, 0xbb, 0xbf}, []byte(strings.Repeat("1:\t原文\r\n", 1800)+"last line")...)
	size := int64(len(data))
	upload, err := s.Upload(ctx, connection, session.ID, bytes.NewReader(data), &size, "", "text/plain")
	if err != nil {
		t.Fatal(err)
	}
	write := map[string]any{"path": path, "source": upload.Ref, "condition": fsp.Condition{Absent: true}}
	complete("op_write", "write", write)
	if r := invoke("op_write", "write", write); !r.OK || wireValue[wire.Operation](t, r.Result).Status != "succeeded" {
		t.Fatalf("completed write replayed: %+v", r)
	}
	src := wireValue[hostcmd.ByteSource](t, json.RawMessage(complete("op_read", "read", map[string]any{"path": path}).Value))
	read := func() {
		t.Helper()
		var got bytes.Buffer
		if err := s.ReadBytes(ctx, connection, session.ID, src.Ref, 0, nil, &got); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got.Bytes(), data) {
			t.Fatal("raw bytes changed")
		}
	}
	read()
	if got, err := os.ReadFile(filepath.Join(outside, "原文.txt")); err != nil || !bytes.Equal(got, data) {
		t.Fatal("did not write the full native path", err)
	}
	meta := wireValue[fsp.Entry](t, json.RawMessage(complete("op_stat", "stat", map[string]any{"path": path}).Value))
	copyPath := location(filepath.Join(outside, "copy.txt"))
	copied := wireValue[struct {
		Entry fsp.Entry `json:"entry"`
	}](t, json.RawMessage(complete("op_copy", "copy", map[string]any{"src": path, "dst": copyPath, "if_version": meta.Version, "condition": fsp.Condition{Absent: true}}).Value))
	moved := wireValue[fsp.Entry](t, json.RawMessage(complete("op_move", "move", map[string]any{"src": copyPath, "dst": location(filepath.Join(outside, "moved.txt")), "if_version": copied.Entry.Version, "condition": fsp.Condition{Absent: true}}).Value))
	complete("op_remove", "remove", map[string]any{"path": moved.Path, "if_version": moved.Version})
	listing := wireValue[struct {
		Entries []fsp.Entry `json:"entries"`
	}](t, json.RawMessage(complete("op_list", "list", map[string]any{"path": location(outside)}).Value))
	if len(listing.Entries) != 1 || listing.Entries[0].Path.String() != path.String() {
		t.Fatalf("native listing: %+v", listing)
	}
	// A parent alias must use both its requested path and its resolved target policy.
	alias := filepath.Join(root, "outside")
	if err := os.Symlink(outside, alias); err != nil {
		t.Fatal(err)
	}
	aliasPath := location(filepath.Join(alias, "原文.txt"))
	complete("op_alias", "read", map[string]any{"path": aliasPath})
	complete("op_alias_list", "list", map[string]any{"path": location(alias)})
	complete("op_mkdir", "mkdir", map[string]any{"path": location(filepath.Join(alias, "nested", "child")), "parents": true})
	if _, err := os.Stat(filepath.Join(outside, "nested", "child")); err != nil {
		t.Fatal("mkdir outside workspace", err)
	}
	s.Disconnect(connection)
	if r := get("op_read"); r.OK {
		t.Fatal("disconnected connection remained authorized")
	}
	connection = admit("pc_2")
	if err := s.ReadBytes(ctx, connection, session.ID, src.Ref, 0, nil, io.Discard); err == nil {
		t.Fatal("unbound connection read session bytes")
	}
	if r := request("session.resume", map[string]any{"session_id": session.ID, "runtime_epoch": session.RuntimeEpoch, "resume_token": session.ResumeToken}); !r.OK {
		t.Fatal(r.Error)
	}
	read()
	updated := cfg.AuthSnapshot()
	updated.FsDeny = []string{filepath.Join(outside, "原文.txt")}
	cfg.SetAuth(updated)
	c.policy.Reconcile()
	if r := invoke("op_denied_alias", "read", map[string]any{"path": aliasPath}); r.OK || r.Error.Code != "permission_denied" {
		t.Fatalf("alias bypassed policy: %+v", r)
	}
	if err := s.ReadBytes(ctx, connection, session.ID, src.Ref, 0, nil, io.Discard); err == nil {
		t.Fatal("live byte source bypassed policy revocation")
	}
	if r := get("op_read"); r.OK || r.Error.Code != "permission_denied" {
		t.Fatalf("retained result bypassed current policy: %+v", r)
	}
	if r := invoke("op_root", "list", map[string]any{"path": fsp.Path{RootID: roots.Roots[0].ID, Segments: []string{}}}); r.OK || r.Error.Code != "permission_denied" {
		t.Fatalf("owner bypassed default closed file policy: %+v", r)
	}

	if r := request("session.close", map[string]any{"session_id": session.ID}); !r.OK {
		t.Fatal(r.Error)
	}
	if err := s.ReadBytes(ctx, connection, session.ID, upload.Ref, 0, nil, io.Discard); err == nil {
		t.Fatal("closed session retained byte access")
	}
}
