package indexer

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
	"time"

	"github.com/veypi/vsh/commands"
)

func TestCandidatesContentFilesAndScope(t *testing.T) {
	root := t.TempDir()
	writeIndexFile(t, filepath.Join(root, "a.go"), "Alpha beta\n")
	writeIndexFile(t, filepath.Join(root, "sub", "b.txt"), "gamma\n")
	writeIndexFile(t, filepath.Join(root, "short.txt"), "x\n")

	s, err := New(Config{Roots: []string{root}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	canonical := filepath.ToSlash(root)
	waitCandidates(t, s, commands.FileIndexQuery{Root: canonical, FilesOnly: true}, func(paths []string) bool {
		return len(paths) == 3
	})

	paths, ok := s.Candidates(context.Background(), commands.FileIndexQuery{Root: canonical, Literals: []string{"alpha"}})
	if !ok || len(paths) != 1 || paths[0] != canonical+"/a.go" {
		t.Fatalf("content candidates = %v, %v", paths, ok)
	}
	paths, ok = s.Candidates(context.Background(), commands.FileIndexQuery{Root: canonical + "/sub", FilesOnly: true})
	if !ok || len(paths) != 1 || paths[0] != canonical+"/sub/b.txt" {
		t.Fatalf("subroot files = %v, %v", paths, ok)
	}
	if _, ok := s.Candidates(context.Background(), commands.FileIndexQuery{Root: canonical, Literals: []string{"x"}}); ok {
		t.Fatal("short literal must fall back to live search")
	}
	if _, ok := s.Candidates(context.Background(), commands.FileIndexQuery{Root: filepath.ToSlash(t.TempDir()), FilesOnly: true}); ok {
		t.Fatal("outside index_roots must fall back to live search")
	}
}

func TestWatcherCreateModifyRenameAndDelete(t *testing.T) {
	root := t.TempDir()
	name := filepath.Join(root, "a.txt")
	writeIndexFile(t, name, "before\n")
	s, err := New(Config{Roots: []string{root}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	canonical := filepath.ToSlash(root)
	waitCandidates(t, s, commands.FileIndexQuery{Root: canonical, FilesOnly: true}, func(paths []string) bool {
		return len(paths) == 1
	})

	writeIndexFile(t, name, "needle content\n")
	waitCandidates(t, s, commands.FileIndexQuery{Root: canonical, Literals: []string{"needle"}}, func(paths []string) bool {
		return slices.Contains(paths, canonical+"/a.txt")
	})
	created := filepath.Join(root, "new", "created.txt")
	writeIndexFile(t, created, "needle in created\n")
	waitCandidates(t, s, commands.FileIndexQuery{Root: canonical, Literals: []string{"needle"}}, func(paths []string) bool {
		return slices.Contains(paths, canonical+"/new/created.txt")
	})

	renamed := filepath.Join(root, "new", "renamed.txt")
	if err := os.Rename(created, renamed); err != nil {
		t.Fatal(err)
	}
	waitCandidates(t, s, commands.FileIndexQuery{Root: canonical, FilesOnly: true}, func(paths []string) bool {
		return slices.Contains(paths, canonical+"/new/renamed.txt") && !slices.Contains(paths, canonical+"/new/created.txt")
	})
	if err := os.Remove(renamed); err != nil {
		t.Fatal(err)
	}
	waitCandidates(t, s, commands.FileIndexQuery{Root: canonical, FilesOnly: true}, func(paths []string) bool {
		return !slices.Contains(paths, canonical+"/new/renamed.txt")
	})
}

func TestSymlinkedRootKeepsCallerPaths(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires additional Windows privileges")
	}
	actual := t.TempDir()
	writeIndexFile(t, filepath.Join(actual, "a.txt"), "needle\n")
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(actual, link); err != nil {
		t.Fatal(err)
	}
	s, err := New(Config{Roots: []string{link}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	canonical := filepath.ToSlash(link)
	waitCandidates(t, s, commands.FileIndexQuery{Root: canonical, Literals: []string{"needle"}}, func(paths []string) bool {
		return slices.Contains(paths, canonical+"/a.txt")
	})
}

func TestHiddenCandidatesRequireOptIn(t *testing.T) {
	root := t.TempDir()
	writeIndexFile(t, filepath.Join(root, "visible.txt"), "needle\n")
	writeIndexFile(t, filepath.Join(root, ".git", "config"), "needle\n")
	if err := os.WriteFile(filepath.Join(root, ".git", "object"), []byte{0xff, 0x00}, 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := New(Config{Roots: []string{root}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	canonical := filepath.ToSlash(root)
	waitCandidates(t, s, commands.FileIndexQuery{Root: canonical, Literals: []string{"needle"}}, func(paths []string) bool {
		return len(paths) == 1 && paths[0] == canonical+"/visible.txt"
	})
	paths, ok := s.Candidates(context.Background(), commands.FileIndexQuery{Root: canonical, Literals: []string{"needle"}, IncludeHidden: true})
	if !ok || len(paths) != 2 {
		t.Fatalf("hidden candidates = %v, %v", paths, ok)
	}
}

func TestSkipsNoiseTreesAndFallsBackForExplicitNoiseRoot(t *testing.T) {
	root := t.TempDir()
	writeIndexFile(t, filepath.Join(root, "visible.txt"), "needle\n")
	writeIndexFile(t, filepath.Join(root, "node_modules", "ignored.txt"), "needle\n")
	writeIndexFile(t, filepath.Join(root, "dist", "ignored.txt"), "needle\n")
	s, err := New(Config{Roots: []string{root}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	canonical := filepath.ToSlash(root)
	waitCandidates(t, s, commands.FileIndexQuery{Root: canonical, Literals: []string{"needle"}}, func(paths []string) bool {
		return slices.Contains(paths, canonical+"/visible.txt")
	})
	paths, ok := s.Candidates(context.Background(), commands.FileIndexQuery{Root: canonical, Literals: []string{"needle"}})
	if !ok || len(paths) != 1 || paths[0] != canonical+"/visible.txt" {
		t.Fatalf("noise trees entered candidates: %v, %v", paths, ok)
	}
	if _, ok := s.Candidates(context.Background(), commands.FileIndexQuery{Root: canonical + "/node_modules", FilesOnly: true}); ok {
		t.Fatal("explicit noise root must use the live walker")
	}
}

func TestUnindexedFileRemainsContentCandidate(t *testing.T) {
	root := t.TempDir()
	writeIndexFile(t, filepath.Join(root, "non-utf8.dat"), string([]byte{'a', 0xff, 'b'}))
	s, err := New(Config{Roots: []string{root}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	canonical := filepath.ToSlash(root)
	waitCandidates(t, s, commands.FileIndexQuery{Root: canonical, FilesOnly: true}, func(paths []string) bool {
		return len(paths) == 1
	})
	paths, ok := s.Candidates(context.Background(), commands.FileIndexQuery{Root: canonical, Literals: []string{"anything"}})
	if !ok || !slices.Contains(paths, canonical+"/non-utf8.dat") {
		t.Fatalf("unindexed file must be rechecked live: %v, %v", paths, ok)
	}
}

func TestBinaryFileIsNotAContentCandidate(t *testing.T) {
	root := t.TempDir()
	writeIndexFile(t, filepath.Join(root, "binary.dat"), string([]byte{'a', 0, 'b'}))
	s, err := New(Config{Roots: []string{root}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	canonical := filepath.ToSlash(root)
	waitCandidates(t, s, commands.FileIndexQuery{Root: canonical, FilesOnly: true}, func(paths []string) bool {
		return len(paths) == 1
	})
	paths, ok := s.Candidates(context.Background(), commands.FileIndexQuery{Root: canonical, Literals: []string{"anything"}})
	if !ok || len(paths) != 0 {
		t.Fatalf("binary file should not be rechecked for content: %v, %v", paths, ok)
	}
}

func waitCandidates(t *testing.T, s *Service, query commands.FileIndexQuery, predicate func([]string) bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		paths, ok := s.Candidates(context.Background(), query)
		if ok && predicate(paths) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	paths, ok := s.Candidates(context.Background(), query)
	t.Fatalf("index condition not reached; last = %v, %v", paths, ok)
}

func writeIndexFile(t *testing.T, name, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
