// Package indexer provides the Pod-resident filename and content candidate index.
// It is an optimization only: callers still apply their filesystem policy and
// read the current file before producing a match.
package indexer

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/veypi/aic-pod/protocol"
	"github.com/veypi/vsh/commands"
)

const maxExtractBytes = 8 << 20

// indexSkipDirs mirrors the unconditional fsx/vsh rg recursion skips. Keeping
// these trees out of the resident index is what makes indexing large workspaces
// bounded; explicitly searching inside one falls back to the live walker.
var indexSkipDirs = map[string]bool{
	"node_modules": true, "vendor": true, "__pycache__": true,
	"bower_components": true, "dist": true, "build": true, "target": true,
	".next": true, ".nuxt": true, "coverage": true, ".turbo": true, ".output": true,
}

// ContentExtractor converts a supported document into searchable text. The
// plain-text extractor is built in; document extractors can be added without
// changing the rg or fs contracts.
type ContentExtractor interface {
	Extract(ctx context.Context, name string, data []byte) (text string, ok bool)
}

type ContentExtractorFunc func(context.Context, string, []byte) (string, bool)

func (f ContentExtractorFunc) Extract(ctx context.Context, name string, data []byte) (string, bool) {
	return f(ctx, name, data)
}

type Config struct {
	Roots      []string
	Extractors []ContentExtractor
	Logf       func(format string, args ...any)
}

type Service struct {
	ctx        context.Context
	cancel     context.CancelFunc
	logf       func(string, ...any)
	extractors []ContentExtractor
	roots      []*indexRoot
	revision   atomic.Uint64
	wg         sync.WaitGroup
}

type fileEntry struct {
	grams     map[uint32]struct{}
	unindexed bool
}

type indexEvent struct {
	Name string
}

type indexWatcher interface {
	Add(string) error
	Close() error
	Events() <-chan indexEvent
	Errors() <-chan error
}

type indexRoot struct {
	service *Service
	path    string
	native  string
	watcher indexWatcher

	mu       sync.RWMutex
	files    map[string]*fileEntry
	postings map[uint32]map[string]struct{}
	ready    bool
	building bool
	dirty    bool
	closed   bool
}

func New(cfg Config) (*Service, error) {
	ctx, cancel := context.WithCancel(context.Background())
	s := &Service{ctx: ctx, cancel: cancel, logf: cfg.Logf}
	s.extractors = append(s.extractors, cfg.Extractors...)
	s.extractors = append(s.extractors, ContentExtractorFunc(plainText))
	if s.logf == nil {
		s.logf = func(string, ...any) {}
	}
	seen := make(map[string]bool)
	for _, configured := range cfg.Roots {
		canonical, native, err := canonicalRoot(configured)
		if err != nil {
			cancel()
			return nil, err
		}
		if canonical == "" {
			continue
		}
		key := canonical
		if runtime.GOOS == "windows" {
			key = strings.ToLower(key)
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		// Resolve only the disk walk root. Public candidates keep the configured
		// path so /tmp and other symlinked roots retain their caller-facing form.
		if resolved, err := filepath.EvalSymlinks(native); err == nil {
			native = resolved
		}
		watcher, err := newIndexWatcher(native)
		if err != nil {
			s.Close()
			return nil, err
		}
		r := &indexRoot{service: s, path: canonical, native: native, watcher: watcher}
		s.roots = append(s.roots, r)
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			r.watch()
		}()
		r.rebuild()
	}
	return s, nil
}

func canonicalRoot(name string) (string, string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", "", nil
	}
	if runtime.GOOS == "windows" {
		if !strings.HasPrefix(name, "/") {
			abs, err := filepath.Abs(name)
			if err != nil {
				return "", "", err
			}
			name = abs
		}
		canonical := protocol.NormalizeHostPath(filepath.ToSlash(name))
		native, err := protocol.HostPathToOS(canonical, os.TempDir())
		return canonical, native, err
	}
	abs, err := filepath.Abs(name)
	if err != nil {
		return "", "", err
	}
	canonical := path.Clean(filepath.ToSlash(abs))
	return canonical, filepath.FromSlash(canonical), nil
}

// IndexRevision changes whenever any indexed tree changes. Search-layer caches
// can use it to invalidate derived ignore rules without a time-based staleness.
func (s *Service) IndexRevision() uint64 {
	if s == nil {
		return 0
	}
	return s.revision.Load()
}

func (s *Service) Close() error {
	if s == nil {
		return nil
	}
	s.cancel()
	for _, root := range s.roots {
		root.mu.Lock()
		root.closed = true
		root.ready = false
		root.mu.Unlock()
		_ = root.watcher.Close()
	}
	s.wg.Wait()
	return nil
}

// Candidates implements commands.FileIndex. An unavailable, incomplete or
// out-of-scope index returns ok=false so rg performs its ordinary live walk.
func (s *Service) Candidates(ctx context.Context, query commands.FileIndexQuery) ([]string, bool) {
	if s == nil || ctx.Err() != nil {
		return nil, false
	}
	root := s.findRoot(query.Root)
	if root == nil || indexSkippedPath(root.path, query.Root) {
		return nil, false
	}
	root.mu.RLock()
	defer root.mu.RUnlock()
	if !root.ready || root.closed {
		return nil, false
	}
	files := root.files
	postings := root.postings
	if query.FilesOnly {
		out := make([]string, 0, len(files))
		for name := range files {
			if indexCandidateVisible(query, name) {
				out = append(out, name)
			}
		}
		sort.Strings(out)
		return out, true
	}

	found := make(map[string]struct{})
	for _, literal := range query.Literals {
		grams := contentGrams(literal)
		if len(grams) == 0 {
			return nil, false
		}
		var smallest map[string]struct{}
		for gram := range grams {
			posting := postings[gram]
			if len(posting) == 0 {
				smallest = nil
				break
			}
			if smallest == nil || len(posting) < len(smallest) {
				smallest = posting
			}
		}
		for name := range smallest {
			entry := files[name]
			if entry == nil || !indexCandidateVisible(query, name) || !hasAllGrams(entry, grams) {
				continue
			}
			found[name] = struct{}{}
		}
	}
	for name, entry := range files {
		if entry.unindexed && indexCandidateVisible(query, name) {
			found[name] = struct{}{}
		}
	}
	out := make([]string, 0, len(found))
	for name := range found {
		out = append(out, name)
	}
	sort.Strings(out)
	return out, true
}

func hasAllGrams(entry *fileEntry, grams map[uint32]struct{}) bool {
	for gram := range grams {
		if _, ok := entry.grams[gram]; !ok {
			return false
		}
	}
	return true
}

func (s *Service) findRoot(target string) *indexRoot {
	target = path.Clean(target)
	var selected *indexRoot
	for _, root := range s.roots {
		if pathWithin(root.path, target) && (selected == nil || len(root.path) > len(selected.path)) {
			selected = root
		}
	}
	return selected
}

func indexWalkError(name, root string, _ fs.DirEntry, err error) error {
	if name != root && (errors.Is(err, fs.ErrPermission) || errors.Is(err, fs.ErrNotExist)) {
		return nil
	}
	return err
}

func indexRelative(root, name string) (string, bool) {
	root = path.Clean(root)
	name = path.Clean(name)
	if !pathWithin(root, name) {
		return "", false
	}
	if len(name) == len(root) {
		return ".", true
	}
	prefix := strings.TrimSuffix(root, "/") + "/"
	if len(name) <= len(prefix) {
		return "", false
	}
	return name[len(prefix):], true
}

func indexSkippedPath(root, name string) bool {
	rel, ok := indexRelative(root, name)
	if !ok || rel == "." {
		return false
	}
	for _, component := range strings.Split(rel, "/") {
		if indexSkipDirs[component] {
			return true
		}
	}
	return false
}

func indexCandidateVisible(query commands.FileIndexQuery, name string) bool {
	rel, ok := indexRelative(query.Root, name)
	if !ok {
		return false
	}
	if query.IncludeHidden || rel == "." {
		return true
	}
	for _, component := range strings.Split(rel, "/") {
		if strings.HasPrefix(component, ".") {
			return false
		}
	}
	return true
}

func pathWithin(root, name string) bool {
	root = path.Clean(root)
	name = path.Clean(name)
	if runtime.GOOS == "windows" {
		root = strings.ToLower(root)
		name = strings.ToLower(name)
	}
	if root == "/" {
		return strings.HasPrefix(name, "/")
	}
	return name == root || strings.HasPrefix(name, strings.TrimSuffix(root, "/")+"/")
}

func (r *indexRoot) rebuild() {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	if r.building {
		r.dirty = true
		r.mu.Unlock()
		return
	}
	r.building = true
	r.ready = false
	r.mu.Unlock()
	r.service.wg.Add(1)
	go func() {
		defer r.service.wg.Done()
		r.build()
	}()
}

func (r *indexRoot) build() {
	files := make(map[string]*fileEntry)
	postings := make(map[uint32]map[string]struct{})
	err := filepath.WalkDir(r.native, func(native string, entry fs.DirEntry, err error) error {
		if err != nil {
			return indexWalkError(native, r.native, entry, err)
		}
		if err := r.service.ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() {
			if native != r.native && indexSkipDirs[entry.Name()] {
				return fs.SkipDir
			}
			// An inaccessible directory is skipped by rg as well. Other watcher
			// failures must not publish a ready index: missed events would turn
			// into false negatives, so fall back to live rg.
			if err := r.watcher.Add(native); err != nil {
				if native != r.native && (errors.Is(err, fs.ErrPermission) || errors.Is(err, fs.ErrNotExist)) {
					return fs.SkipDir
				}
				return err
			}
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 || !entry.Type().IsRegular() {
			return nil
		}
		canonical := r.canonical(native)
		file := r.extract(native)
		files[canonical] = file
		for gram := range file.grams {
			posting := postings[gram]
			if posting == nil {
				posting = make(map[string]struct{})
				postings[gram] = posting
			}
			posting[canonical] = struct{}{}
		}
		return nil
	})
	if err != nil && r.service.ctx.Err() == nil {
		r.service.logf("index: build %s: %v", r.path, err)
	}

	r.mu.Lock()
	if err == nil && !r.closed {
		r.files = files
		r.postings = postings
		r.ready = true
		r.service.revision.Add(1)
	}
	r.building = false
	again := r.dirty && !r.closed
	r.dirty = false
	if again {
		r.building = true
		r.ready = false
	}
	r.mu.Unlock()
	if again {
		r.service.wg.Add(1)
		go func() {
			defer r.service.wg.Done()
			r.build()
		}()
	}
}

func (r *indexRoot) canonical(native string) string {
	rel, err := filepath.Rel(r.native, native)
	if err != nil {
		return r.path
	}
	if rel == "." {
		return r.path
	}
	return path.Join(r.path, filepath.ToSlash(rel))
}

func (r *indexRoot) extract(native string) *fileEntry {
	info, err := os.Stat(native)
	if err != nil {
		return &fileEntry{unindexed: true}
	}
	if info.Size() > maxExtractBytes {
		return &fileEntry{unindexed: true}
	}
	data, err := os.ReadFile(native)
	if err != nil {
		return &fileEntry{unindexed: true}
	}
	for _, extractor := range r.extractors() {
		text, ok := extractor.Extract(r.service.ctx, native, data)
		if ok {
			return &fileEntry{grams: contentGrams(text)}
		}
	}
	// rg skips NUL-containing binary data. It is therefore a known non-match,
	// not an unindexed file that every content query must read again.
	if bytes.IndexByte(data, 0) >= 0 {
		return &fileEntry{}
	}
	return &fileEntry{unindexed: true}
}

func (r *indexRoot) extractors() []ContentExtractor {
	return r.service.extractors
}

func plainText(_ context.Context, _ string, data []byte) (string, bool) {
	if !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
		return "", false
	}
	return string(data), true
}

func contentGrams(text string) map[uint32]struct{} {
	runes := []rune(strings.ToLower(text))
	if len(runes) < 3 {
		return nil
	}
	out := make(map[uint32]struct{}, len(runes)-2)
	for i := 0; i+2 < len(runes); i++ {
		h := uint32(runes[i])<<24 ^ uint32(runes[i+1])<<12 ^ uint32(runes[i+2])
		out[h] = struct{}{}
	}
	return out
}

func (r *indexRoot) watch() {
	changes := make(map[string]struct{})
	var timer *time.Timer
	var pending <-chan time.Time
	for {
		select {
		case <-r.service.ctx.Done():
			if timer != nil {
				timer.Stop()
			}
			return
		case _, ok := <-r.watcher.Errors():
			if !ok {
				return
			}
			r.rebuild()
		case event, ok := <-r.watcher.Events():
			if !ok {
				return
			}
			r.mu.Lock()
			building := r.building
			if building {
				r.dirty = true
			}
			r.mu.Unlock()
			if building {
				continue
			}
			changes[event.Name] = struct{}{}
			if timer == nil {
				timer = time.NewTimer(150 * time.Millisecond)
			} else {
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(150 * time.Millisecond)
			}
			pending = timer.C
		case <-pending:
			batch := changes
			changes = make(map[string]struct{})
			pending = nil
			if !r.apply(batch) {
				r.rebuild()
			}
		}
	}
}

func (r *indexRoot) apply(changes map[string]struct{}) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.ready || r.closed {
		return true
	}
	for native := range changes {
		canonical := r.canonical(native)
		r.remove(canonical)
		info, err := os.Lstat(native)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return false
		}
		if info.Mode()&os.ModeSymlink != 0 {
			continue
		}
		if info.IsDir() {
			if !r.addTree(native) {
				return false
			}
			continue
		}
		if info.Mode().IsRegular() {
			r.addFile(native, canonical)
		}
	}
	r.service.revision.Add(1)
	return true
}

func (r *indexRoot) addTree(native string) bool {
	if indexSkippedPath(r.path, r.canonical(native)) {
		return true
	}
	ok := true
	err := filepath.WalkDir(native, func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			if walkErr := indexWalkError(name, r.native, entry, err); walkErr != nil {
				ok = false
				return walkErr
			}
			return nil
		}
		if entry.IsDir() {
			if name != native && indexSkipDirs[entry.Name()] {
				return fs.SkipDir
			}
			if err := r.watcher.Add(name); err != nil {
				if errors.Is(err, fs.ErrPermission) || errors.Is(err, fs.ErrNotExist) {
					return fs.SkipDir
				}
				ok = false
				return err
			}
			return nil
		}
		if entry.Type().IsRegular() && entry.Type()&os.ModeSymlink == 0 {
			r.addFile(name, r.canonical(name))
		}
		return nil
	})
	return err == nil && ok
}

func (r *indexRoot) addFile(native, canonical string) {
	file := r.extract(native)
	r.files[canonical] = file
	for gram := range file.grams {
		posting := r.postings[gram]
		if posting == nil {
			posting = make(map[string]struct{})
			r.postings[gram] = posting
		}
		posting[canonical] = struct{}{}
	}
}

func (r *indexRoot) remove(canonical string) {
	for name, entry := range r.files {
		if !pathWithin(canonical, name) {
			continue
		}
		for gram := range entry.grams {
			delete(r.postings[gram], name)
			if len(r.postings[gram]) == 0 {
				delete(r.postings, gram)
			}
		}
		delete(r.files, name)
	}
}

var _ commands.FileIndex = (*Service)(nil)
