package fsx

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/veypi/vsh/commands"
)

type stubFileIndex struct {
	paths []string
	ok    bool
}

func (s *stubFileIndex) Candidates(context.Context, commands.FileIndexQuery) ([]string, bool) {
	return append([]string(nil), s.paths...), s.ok
}

func TestRgIndexFiltersPathsAndPolicies(t *testing.T) {
	env := newTestEnv(t)
	runWrite(t, env, "/a/hit.txt", "needle\n")
	runWrite(t, env, "/node_modules/skip.txt", "needle\n")
	runWrite(t, env, "/denied.txt", "needle\n")
	counter := &readDirCountFS{FS: env.FS, reads: map[string]int{}}
	env.FS = counter
	env.FileIndex = &stubFileIndex{ok: true, paths: []string{"/a/hit.txt", "/node_modules/skip.txt", "/denied.txt", "/gone.txt"}}
	env.Gate = func(_ string, name string, _ bool) error {
		if name == "/denied.txt" {
			return errors.New("denied")
		}
		return nil
	}

	r := runFS(t, env, map[string]any{"action": "rg", "path": "/", "pattern": "needle"})
	var body struct {
		Files []struct {
			Path string `json:"path"`
		} `json:"files"`
	}
	if err := json.Unmarshal([]byte(r.Content), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Files) != 1 || body.Files[0].Path != "/a/hit.txt" {
		t.Fatalf("indexed content result = %s", r.Content)
	}
	if len(counter.reads) != 0 {
		t.Fatalf("indexed content unexpectedly walked directories: %v", counter.reads)
	}

	r = runFS(t, env, map[string]any{"action": "rg", "path": "/"})
	var listed struct {
		Files []string `json:"files"`
	}
	if err := json.Unmarshal([]byte(r.Content), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Files) != 1 || listed.Files[0] != "/a/hit.txt" {
		t.Fatalf("indexed file result = %s", r.Content)
	}
}

func TestRgIndexUnavailableFallsBack(t *testing.T) {
	env := newTestEnv(t)
	runWrite(t, env, "/live.txt", "needle\n")
	env.FileIndex = &stubFileIndex{ok: false}
	r := runFS(t, env, map[string]any{"action": "rg", "path": "/", "pattern": "needle"})
	var body struct {
		Files []struct {
			Path string `json:"path"`
		} `json:"files"`
	}
	if err := json.Unmarshal([]byte(r.Content), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Files) != 1 || body.Files[0].Path != "/live.txt" {
		t.Fatalf("live fallback = %s", r.Content)
	}
}
