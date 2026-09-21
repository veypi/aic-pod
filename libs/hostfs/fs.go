// Package hostfs implements the structured fs/1 provider. Bytes are returned as
// session-scoped sources, never as formatted tool text. Unsupported semantics
// are rejected instead of being approximated by a lossy legacy adapter.
package hostfs

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	tool "github.com/veypi/aic-pod/libs/hosts_tool"
	fsp "github.com/veypi/aic-pod/protocol/fs"
	hosts "github.com/veypi/aic-pod/protocol/fs"
	wire "github.com/veypi/aic-pod/protocol/hosts_tools"
)

type Root struct {
	ID, Name, Path string
	Default        bool
}
type Config struct {
	Roots []Root
	// Home is the initial browsing directory, not a filesystem boundary.
	Home  *fsp.Path
	Bytes *Bytes
	// Check must consult the current device policy; it must not trust args.
	Check               func(context.Context, Call, string, bool) error
	MaxProxyUploadBytes int64
	MaxDirectoryEntries int
}
type root struct {
	Root
	handle *os.Root
}
type FS struct {
	cfg    Config
	roots  map[string]*root
	mu     sync.Mutex
	closed bool
}

func New(cfg Config) (*FS, error) {
	if cfg.Bytes == nil || cfg.Check == nil || len(cfg.Roots) == 0 {
		return nil, fmt.Errorf("hostfs: roots, byte store and policy check required")
	}
	if cfg.MaxProxyUploadBytes <= 0 {
		cfg.MaxProxyUploadBytes = 64 << 20
	}
	if cfg.MaxDirectoryEntries <= 0 {
		cfg.MaxDirectoryEntries = 10000
	}
	f := &FS{cfg: cfg, roots: map[string]*root{}}
	defaults := 0
	for _, r := range cfg.Roots {
		if !hosts.ValidID(r.ID) || f.roots[r.ID] != nil {
			f.Close()
			return nil, fmt.Errorf("invalid or duplicate root ID")
		}
		if r.Default {
			defaults++
		}
		abs, err := filepath.Abs(r.Path)
		if err == nil {
			abs, err = filepath.EvalSymlinks(abs)
		}
		if err != nil {
			f.Close()
			return nil, err
		}
		r.Path = abs
		handle, err := os.OpenRoot(abs)
		if err != nil {
			f.Close()
			return nil, err
		}
		f.roots[r.ID] = &root{Root: r, handle: handle}
	}
	if defaults != 1 {
		f.Close()
		return nil, fmt.Errorf("hostfs: exactly one default root required")
	}
	if cfg.Home != nil {
		if err := cfg.Home.Validate(runtime.GOOS == "windows"); err != nil || f.roots[cfg.Home.RootID] == nil {
			f.Close()
			return nil, fmt.Errorf("hostfs: invalid home directory")
		}
	}
	return f, nil
}
func (f *FS) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return nil
	}
	f.closed = true
	for _, r := range f.roots {
		r.handle.Close()
	}
	return nil
}

type pathArgs struct {
	Path        fsp.Path `json:"path"`
	IfVersion   string   `json:"if_version,omitempty"`
	Follow      bool     `json:"follow_symlinks,omitempty"`
	Consistency string   `json:"consistency,omitempty"`
}
type listArgs struct {
	Path   fsp.Path `json:"path"`
	Limit  int      `json:"limit,omitempty"`
	Cursor string   `json:"cursor,omitempty"`
	Hidden bool     `json:"hidden,omitempty"`
	Sort   string   `json:"sort,omitempty"`
}
type writeArgs struct {
	Path          fsp.Path          `json:"path"`
	Source        hosts.ResourceRef `json:"source"`
	Condition     fsp.Condition     `json:"condition"`
	CreateParents bool              `json:"create_parents,omitempty"`
}
type mkdirArgs struct {
	Path    fsp.Path `json:"path"`
	Parents bool     `json:"parents,omitempty"`
	ExistOK bool     `json:"exist_ok,omitempty"`
}
type removeArgs struct {
	Path      fsp.Path `json:"path"`
	IfVersion string   `json:"if_version"`
	Recursive bool     `json:"recursive,omitempty"`
	MissingOK bool     `json:"missing_ok,omitempty"`
}

func objectSchema(properties map[string]any, required ...string) json.RawMessage {
	raw, _ := json.Marshal(map[string]any{"type": "object", "properties": properties, "required": required, "additionalProperties": false})
	return raw
}
func (f *FS) Methods() []tool.Method {
	path := map[string]any{"type": "object", "required": []string{"root_id", "segments"}, "properties": map[string]any{"root_id": map[string]any{"type": "string"}, "segments": map[string]any{"type": "array", "items": map[string]any{"type": "string"}}}, "additionalProperties": false}
	text := map[string]any{"type": "string"}
	boolean := map[string]any{"type": "boolean"}
	methods := map[string]fsMethod{
		"roots":  {InputSchema: objectSchema(map[string]any{}), Effect: "read"},
		"home":   {InputSchema: objectSchema(map[string]any{}), Effect: "read"},
		"stat":   {InputSchema: objectSchema(map[string]any{"path": path, "if_version": text, "follow_symlinks": map[string]any{"const": false}}, "path"), Effect: "read"},
		"read":   {InputSchema: objectSchema(map[string]any{"path": path, "if_version": text, "consistency": map[string]any{"enum": []string{"verified"}}, "follow_symlinks": map[string]any{"const": false}}, "path"), Effect: "read"},
		"list":   {InputSchema: objectSchema(map[string]any{"path": path, "limit": map[string]any{"type": "integer", "minimum": 1, "maximum": 1000}, "cursor": text, "hidden": boolean, "sort": map[string]any{"enum": []string{"name"}}}, "path"), Effect: "read"},
		"write":  {InputSchema: objectSchema(map[string]any{"path": path, "source": map[string]any{"type": "object"}, "condition": map[string]any{"type": "object"}, "create_parents": map[string]any{"const": false}}, "path", "source", "condition"), Effect: "write"},
		"mkdir":  {InputSchema: objectSchema(map[string]any{"path": path, "parents": boolean, "exist_ok": boolean}, "path"), Effect: "write"},
		"remove": {InputSchema: objectSchema(map[string]any{"path": path, "if_version": text, "recursive": boolean, "missing_ok": boolean}, "path", "if_version"), Effect: "write"},
	}
	methods["find"] = fsMethod{InputSchema: objectSchema(map[string]any{"path": path, "glob": text, "depth": map[string]any{"type": "integer", "minimum": 0, "maximum": 64}, "limit": map[string]any{"type": "integer", "minimum": 1, "maximum": 1000}}, "path"), Effect: "read"}
	methods["move"] = fsMethod{InputSchema: objectSchema(map[string]any{"src": path, "dst": path, "if_version": text, "condition": map[string]any{"type": "object"}}, "src", "dst", "if_version", "condition"), Effect: "write"}
	methods["copy"] = methods["move"]
	if !safeReadSupported() {
		delete(methods, "copy")
		delete(methods, "read")
	}
	if !atomicReplaceSupported() {
		delete(methods, "write")
		delete(methods, "move")
		delete(methods, "copy")
	}
	out := []tool.Method{}
	for name, desc := range methods {
		level := 1
		if desc.Effect == "write" {
			level = 2
		}
		out = append(out, tool.Method{Descriptor: wire.Method{Name: name, Mode: wire.Call, Access: level, Input: desc.InputSchema}, Run: func(ctx context.Context, caller tool.Caller, args json.RawMessage) (any, error) {
			return f.Run(ctx, Call{Caller: caller, Owner: Owner(caller), Command: "fs", Method: name, Args: args})
		}})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Descriptor.Name < out[j].Descriptor.Name })
	return append(out, f.byteMethods()...)
}
func (f *FS) validate(method string, raw json.RawMessage) error {
	var path fsp.Path
	switch method {
	case "roots", "home":
		var p struct{}
		return hosts.Decode(raw, &p)
	case "stat", "read":
		var p pathArgs
		if err := hosts.Decode(raw, &p); err != nil {
			return err
		}
		path = p.Path
		if p.Follow {
			return hosts.Fail("unsupported", "Following symbolic links is not available")
		}
		if p.Consistency != "" && (method != "read" || p.Consistency != "verified") {
			return hosts.Fail("unsupported", "Requested read consistency is unavailable")
		}
	case "list":
		var p listArgs
		if err := hosts.Decode(raw, &p); err != nil {
			return err
		}
		path = p.Path
		if p.Limit < 0 || p.Limit > 1000 || len(p.Cursor) > 2048 {
			return hosts.Fail("invalid_argument", "Invalid list bounds")
		}
		if p.Sort != "" && p.Sort != "name" {
			return hosts.Fail("unsupported", "Unsupported directory sort")
		}
	case "write":
		var p writeArgs
		if err := hosts.Decode(raw, &p); err != nil {
			return err
		}
		path = p.Path
		if err := p.Condition.Validate(); err != nil {
			return err
		}
		if p.CreateParents {
			return hosts.Fail("unsupported", "Create the parent directory explicitly")
		}
		if p.Source.Kind != "bytes" || !hosts.ValidID(p.Source.ID) || !hosts.ValidID(p.Source.Epoch) {
			return hosts.Fail("invalid_argument", "Invalid byte source")
		}
	case "find":
		var p findArgs
		if err := hosts.Decode(raw, &p); err != nil {
			return err
		}
		path = p.Path
		if p.Depth < 0 || p.Depth > 64 || p.Limit < 0 || p.Limit > 1000 || len(p.Glob) > 256 || strings.ContainsAny(p.Glob, `/\[]{}`) {
			return hosts.Fail("invalid_argument", "Invalid find bounds or basename glob")
		}
	case "move", "copy":
		var p moveArgs
		if err := hosts.Decode(raw, &p); err != nil {
			return err
		}
		if p.IfVersion == "" {
			return hosts.Fail("invalid_argument", "if_version is required")
		}
		if err := p.Condition.Validate(); err != nil {
			return err
		}
		if err := p.Src.Validate(runtime.GOOS == "windows"); err != nil {
			return err
		}
		path = p.Dst
	case "mkdir":
		var p mkdirArgs
		if err := hosts.Decode(raw, &p); err != nil {
			return err
		}
		path = p.Path
	case "remove":
		var p removeArgs
		if err := hosts.Decode(raw, &p); err != nil {
			return err
		}
		path = p.Path
		if p.IfVersion == "" {
			return hosts.Fail("invalid_argument", "if_version is required")
		}
	default:
		return hosts.Fail("unsupported", "Unknown fs method")
	}
	return path.Validate(runtime.GOOS == "windows")
}

// Authorize applies current policy both to new invocations and to queries of
// retained results. Execution and range reads also check at the point of use.
func (f *FS) Authorize(ctx context.Context, call Call) error {
	if call.Command != "fs" {
		return hosts.Fail("unsupported", "Command is not registered")
	}
	if err := f.validate(call.Method, call.Args); err != nil {
		return err
	}
	if call.Method == "roots" || call.Method == "home" {
		return nil // Mount metadata grants no access; home checks its actual path.
	}
	var p struct {
		Path    fsp.Path `json:"path"`
		Src     fsp.Path `json:"src"`
		Dst     fsp.Path `json:"dst"`
		Parents bool     `json:"parents"`
	}
	if err := json.Unmarshal(call.Args, &p); err != nil {
		return hosts.Fail("invalid_argument", "Invalid file location")
	}
	write := call.Method == "write" || call.Method == "mkdir" || call.Method == "remove" || call.Method == "move" || call.Method == "copy"
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return hosts.Fail("expired", "Filesystem closed")
	}
	if call.Method == "move" || call.Method == "copy" {
		if _, _, err := f.check(ctx, call, p.Src, call.Method == "move"); err != nil {
			return fault(err)
		}
		p.Path = p.Dst
	}
	// mkdir -p 的写授权按「实际创建的层级」在执行内逐层判定（mkdirParents
	// 先检后建、拒绝时零副作用）：整目标在此不做写检查——已存在的祖先不
	// 需要写授权，否则 write/curl -o 等补父目录的便利逻辑会把授权要求放大
	// 到容器目录，与 edit/mkdir/remove 只查目标的口径不一致。
	if call.Method == "mkdir" && p.Parents {
		return nil
	}
	_, _, err := f.check(ctx, call, p.Path, write)
	if err != nil {
		return fault(err)
	}
	return nil
}
func (f *FS) run(ctx context.Context, call Call) (value any, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	defer func() {
		if err != nil {
			err = fault(err)
		}
	}()
	if f.closed {
		return nil, hosts.Fail("expired", "Filesystem closed")
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	switch call.Method {
	case "home":
		p := f.cfg.Home
		if p == nil {
			for _, r := range f.roots {
				if r.Default {
					p = &fsp.Path{RootID: r.ID, Segments: []string{}}
					break
				}
			}
		}
		if _, _, err := f.check(ctx, call, *p, false); err != nil {
			return nil, err
		}
		return p, nil
	case "roots":
		out := make([]map[string]any, 0, len(f.roots))
		for _, r := range f.roots {
			// A volume may contain allowed descendants even when listing its root
			// is denied. Keep addressing available; each operation checks its path.
			style := "posix"
			if runtime.GOOS == "windows" {
				style = "windows"
			}
			out = append(out, map[string]any{"id": r.ID, "name": r.Name, "default": r.Default, "native_path": filepath.ToSlash(r.Path), "path_style": style, "features": map[string]any{"read": safeReadSupported(), "write": atomicReplaceSupported() && f.cfg.Check(ctx, call, r.Path, true) == nil, "atomic_replace": atomicReplaceSupported(), "conditional_write": "runtime_serialized"}, "limits": map[string]int{"directory_entries": f.cfg.MaxDirectoryEntries}})
		}
		sort.Slice(out, func(i, j int) bool { return out[i]["id"].(string) < out[j]["id"].(string) })
		return map[string]any{"roots": out}, nil
	case "stat", "read":
		var p pathArgs
		_ = hosts.Decode(call.Args, &p)
		return f.readOrStat(ctx, call, p)
	case "list":
		var p listArgs
		_ = hosts.Decode(call.Args, &p)
		return f.list(ctx, call, p)
	case "write":
		var p writeArgs
		_ = hosts.Decode(call.Args, &p)
		return f.write(ctx, call, p)
	case "find":
		var p findArgs
		_ = hosts.Decode(call.Args, &p)
		return f.find(ctx, call, p)
	case "copy":
		var p moveArgs
		_ = hosts.Decode(call.Args, &p)
		return f.copy(ctx, call, p)
	case "move":
		var p moveArgs
		_ = hosts.Decode(call.Args, &p)
		return f.move(ctx, call, p)
	case "mkdir":
		var p mkdirArgs
		_ = hosts.Decode(call.Args, &p)
		return f.mkdir(ctx, call, p)
	case "remove":
		var p removeArgs
		_ = hosts.Decode(call.Args, &p)
		return f.remove(ctx, call, p)
	}
	return nil, hosts.Fail("unsupported", "Unknown fs method")
}
func fault(err error) error {
	var f *hosts.Fault
	if errors.As(err, &f) {
		return f
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if errors.Is(err, fs.ErrNotExist) {
		return hosts.Fail("not_found", "File or directory does not exist")
	}
	if errors.Is(err, fs.ErrExist) {
		return hosts.Fail("already_exists", "File or directory already exists")
	}
	if errors.Is(err, fs.ErrPermission) {
		return hosts.Fail("permission_denied", "Filesystem access denied")
	}
	return hosts.Fail("filesystem_error", "Filesystem operation could not be completed")
}
func (f *FS) check(ctx context.Context, call Call, p fsp.Path, write bool) (*root, string, error) {
	r := f.roots[p.RootID]
	if r == nil {
		return nil, "", hosts.Fail("not_found", "Unknown filesystem root")
	}
	abs := filepath.Join(append([]string{r.Path}, p.Segments...)...)
	if err := f.cfg.Check(ctx, call, abs, write); err != nil {
		return nil, "", err
	}
	_, resolved, err := f.locate(p)
	if err != nil {
		return nil, "", err
	}
	if resolved != abs {
		if err = f.cfg.Check(ctx, call, resolved, write); err != nil {
			return nil, "", err
		}
	}
	return r, resolved, nil
}

// locate 是 check 去掉策略门控的部分：根身份校验、父链符号链接解析与
// root 收容。策略判定只在 check 内发生；mkdirParents 的存在性探测走本层，
// 已存在的祖先不因 -p 便利逻辑被要求授权。
func (f *FS) locate(p fsp.Path) (*root, string, error) {
	r := f.roots[p.RootID]
	if r == nil {
		return nil, "", hosts.Fail("not_found", "Unknown filesystem root")
	}
	current, err := os.Stat(r.Path)
	if err != nil {
		return nil, "", err
	}
	opened, err := r.handle.Stat(".")
	if err != nil {
		return nil, "", err
	}
	if !os.SameFile(current, opened) {
		return nil, "", hosts.Fail("expired", "Filesystem root identity changed")
	}
	resolved := filepath.Join(append([]string{r.Path}, p.Segments...)...)
	if len(p.Segments) > 0 {
		resolved, err = resolveParentPath(resolved)
		if err != nil {
			return nil, "", err
		}
	}
	rel, err := filepath.Rel(r.Path, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, "", hosts.Fail("permission_denied", "Path is outside the filesystem root")
	}
	return r, resolved, nil
}

// probe 是 mkdirParents 的存在性探测：返回末段最终信息（跟随符号链接——
// mkdir -p 穿过已存在的链接层级），不做策略门控。探测只决定「哪几级缺失」；
// 创建动作自身仍经 check（写）逐层判定。
func (f *FS) probe(p fsp.Path) (fs.FileInfo, error) {
	r, resolved, err := f.locate(p)
	if err != nil {
		return nil, err
	}
	h, name, err := parent(r, resolved)
	if err != nil {
		return nil, err
	}
	defer h.Close()
	info, err := h.Lstat(name)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		return info, err
	}
	full, err := filepath.EvalSymlinks(resolved)
	if err != nil {
		return nil, err
	}
	rel, err := filepath.Rel(r.Path, full)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, hosts.Fail("permission_denied", "Path is outside the filesystem root")
	}
	h2, name2, err := parent(r, full)
	if err != nil {
		return nil, err
	}
	defer h2.Close()
	return h2.Lstat(name2)
}

// Resolve existing parent aliases (for example macOS /var -> /private/var).
// The final component is never followed: remove/move still act on the link itself.
// Missing suffixes remain exact so mkdir/copy admission can validate new paths.
func resolveParentPath(abs string) (string, error) {
	parent := filepath.Dir(abs)
	tail := []string{filepath.Base(abs)}
	for {
		resolved, err := filepath.EvalSymlinks(parent)
		if err == nil {
			return filepath.Join(append([]string{resolved}, tail...)...), nil
		}
		if !errors.Is(err, fs.ErrNotExist) || parent == filepath.Dir(parent) {
			return "", err
		}
		tail = append([]string{filepath.Base(parent)}, tail...)
		parent = filepath.Dir(parent)
	}
}

// Pin the already resolved and authorized path without following any more links.
// Replacements with symlinks between resolution and open are rejected.
func parent(r *root, abs string) (*os.Root, string, error) {
	rel, err := filepath.Rel(r.Path, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, "", hosts.Fail("permission_denied", "Path is outside the filesystem root")
	}
	h, err := r.handle.OpenRoot(".")
	if err != nil {
		return nil, "", err
	}
	if rel == "." {
		return h, ".", nil
	}
	parts := strings.Split(rel, string(filepath.Separator))
	for _, part := range parts[:len(parts)-1] {
		info, e := h.Lstat(part)
		if e != nil {
			h.Close()
			return nil, "", e
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			h.Close()
			return nil, "", hosts.Fail("source_changed", "Directory changed during lookup")
		}
		next, e := h.OpenRoot(part)
		if e != nil {
			h.Close()
			return nil, "", e
		}
		actual, e := next.Stat(".")
		h.Close()
		if e != nil || !os.SameFile(info, actual) {
			next.Close()
			return nil, "", hosts.Fail("source_changed", "Directory changed during lookup")
		}
		h = next
	}
	return h, parts[len(parts)-1], nil
}
func entry(p fsp.Path, info fs.FileInfo) fsp.Entry {
	kind := "other"
	var size *int64
	if info.Mode()&os.ModeSymlink != 0 {
		kind = "symlink"
	} else if info.IsDir() {
		kind = "directory"
	} else if info.Mode().IsRegular() {
		kind = "file"
		n := info.Size()
		size = &n
	}
	media := ""
	if kind == "file" {
		media = mime.TypeByExtension(filepath.Ext(info.Name()))
		if media == "" {
			media = "application/octet-stream"
		}
	}
	return fsp.Entry{Path: p, Name: info.Name(), Kind: kind, Size: size, ModifiedAt: info.ModTime().UTC().Format(time.RFC3339Nano), Version: version(info), MediaType: media}
}
func version(info fs.FileInfo) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d/%d/%d/%s", info.Size(), info.ModTime().UnixNano(), info.Mode(), fileIdentity(info))))
	return "fv_" + hex.EncodeToString(sum[:])
}
func (f *FS) readOrStat(ctx context.Context, call Call, p pathArgs) (any, error) {
	r, abs, err := f.check(ctx, call, p.Path, false)
	if err != nil {
		return nil, err
	}
	h, name, err := parent(r, abs)
	if err != nil {
		return nil, err
	}
	defer h.Close()
	info, err := h.Lstat(name)
	if err != nil {
		return nil, err
	}
	e := entry(p.Path, info)
	if p.IfVersion != "" && p.IfVersion != e.Version {
		return nil, hosts.Fail("version_conflict", "File version changed")
	}
	if call.Method == "stat" {
		return e, nil
	}
	if !info.Mode().IsRegular() {
		return nil, hosts.Fail("unsupported", "read requires a regular file, not a directory or link")
	}
	file, err := openRegular(h, name)
	if err != nil {
		return nil, err
	}
	actual, err := file.Stat()
	if err != nil || !os.SameFile(info, actual) || version(actual) != e.Version {
		file.Close()
		return nil, hosts.Fail("source_changed", "File changed while being opened")
	}
	requested := filepath.Join(append([]string{r.Path}, p.Path.Segments...)...)
	src, err := f.cfg.Bytes.AddFile(call.Owner, file, info.Size(), e.Version, e.MediaType, func(ctx context.Context) error {
		if err := f.cfg.Check(ctx, call, requested, false); err != nil {
			return err
		}
		if err := f.cfg.Check(ctx, call, abs, false); err != nil {
			return err
		}
		now, err := file.Stat()
		if err != nil {
			return err
		}
		if version(now) != e.Version {
			return hosts.Fail("source_changed", "File changed during range read")
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return src, nil
}

type cursor struct {
	Query   string `json:"q"`
	Version string `json:"v"`
	Offset  int    `json:"o"`
}

// Directory browsing may follow a directory alias, but authorizes both names
// and opens the resolved target through pinned, no-follow directory handles.
func (f *FS) directory(ctx context.Context, call Call, p fsp.Path, write bool) (*os.Root, fs.FileInfo, error) {
	r, abs, err := f.check(ctx, call, p, write)
	if err != nil {
		return nil, nil, err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, nil, err
	}
	if err = f.cfg.Check(ctx, call, resolved, write); err != nil {
		return nil, nil, err
	}
	h, name, err := parent(r, resolved)
	if err != nil {
		return nil, nil, err
	}
	defer h.Close()
	info, err := h.Lstat(name)
	if err != nil {
		return nil, nil, err
	}
	if !info.IsDir() {
		return nil, nil, hosts.Fail("invalid_argument", "A directory is required")
	}
	dir, err := h.OpenRoot(name)
	if err != nil {
		return nil, nil, err
	}
	actual, err := dir.Stat(".")
	if err != nil || !os.SameFile(info, actual) {
		dir.Close()
		return nil, nil, hosts.Fail("source_changed", "Directory changed during lookup")
	}
	return dir, info, nil
}

func (f *FS) list(ctx context.Context, call Call, p listArgs) (any, error) {
	dir, info, err := f.directory(ctx, call, p.Path, false)
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	file, err := dir.Open(".")
	if err != nil {
		return nil, err
	}
	defer file.Close()
	entries, err := file.ReadDir(f.cfg.MaxDirectoryEntries + 1)
	if err != nil && err != io.EOF {
		return nil, err
	}
	if len(entries) > f.cfg.MaxDirectoryEntries {
		return nil, hosts.Fail("overloaded", "Directory exceeds this backend's enumeration limit")
	}
	revision := version(info)
	queryRaw, _ := json.Marshal(struct {
		Path   fsp.Path
		Hidden bool
	}{p.Path, p.Hidden})
	queryHash := sha256.Sum256(queryRaw)
	query := hex.EncodeToString(queryHash[:])
	start := 0
	if p.Cursor != "" {
		raw, e := base64.RawURLEncoding.DecodeString(p.Cursor)
		var c cursor
		if e != nil || hosts.Decode(raw, &c) != nil || c.Query != query || c.Version != revision || c.Offset < 0 {
			return nil, hosts.Fail("cursor_invalid", "Directory cursor is stale or has different options")
		}
		start = c.Offset
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	out := make([]fsp.Entry, 0, min(len(entries), 1000))
	visible := 0
	limit := p.Limit
	if limit == 0 {
		limit = 100
	}
	next := ""
	for _, item := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !p.Hidden && strings.HasPrefix(item.Name(), ".") {
			continue
		}
		child := fsp.Path{RootID: p.Path.RootID, Segments: append(append([]string{}, p.Path.Segments...), item.Name())}
		if _, _, e := f.check(ctx, call, child, false); e != nil {
			continue
		}
		if visible < start {
			visible++
			continue
		}
		if len(out) == limit {
			raw, _ := json.Marshal(cursor{Query: query, Version: revision, Offset: visible})
			next = base64.RawURLEncoding.EncodeToString(raw)
			break
		}
		stat, e := dir.Lstat(item.Name())
		if e != nil {
			return nil, e
		}
		out = append(out, entry(child, stat))
		visible++
	}
	after, err := dir.Stat(".")
	if err != nil {
		return nil, err
	}
	if version(after) != revision {
		return nil, hosts.Fail("cursor_invalid", "Directory changed during enumeration")
	}
	return map[string]any{"entries": out, "next_cursor": next, "revision": revision}, nil
}
func (f *FS) write(ctx context.Context, call Call, p writeArgs) (any, error) {
	if len(p.Path.Segments) == 0 {
		return nil, hosts.Fail("permission_denied", "Cannot replace a root")
	}
	if !atomicReplaceSupported() {
		return nil, hosts.Fail("unsupported", "Atomic replacement is unavailable")
	}
	r, abs, err := f.check(ctx, call, p.Path, true)
	if err != nil {
		return nil, err
	}
	src, err := f.cfg.Bytes.Describe(call.Owner, p.Source)
	if err != nil {
		return nil, err
	}
	if !src.Immutable {
		return nil, hosts.Fail("invalid_argument", "write requires a sealed immutable byte source")
	}
	h, name, err := parent(r, abs)
	if err != nil {
		return nil, err
	}
	defer h.Close()
	existing, err := h.Lstat(name)
	exists := err == nil
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	oldVersion := ""
	mode := fs.FileMode(0o600)
	if exists {
		if !existing.Mode().IsRegular() {
			return nil, hosts.Fail("unsupported", "Cannot replace a directory or link")
		}
		oldVersion = version(existing)
		mode = existing.Mode().Perm()
	}
	if err = p.Condition.Check(oldVersion, exists); err != nil {
		return nil, err
	}
	temp, err := hosts.NewID(".aic-write-")
	if err != nil {
		return nil, err
	}
	file, err := h.OpenFile(temp, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	defer func() { file.Close(); h.Remove(temp) }()
	if err = f.cfg.Bytes.Copy(ctx, call.Owner, p.Source, 0, nil, file); err != nil {
		return nil, err
	}
	if err = file.Chmod(mode); err != nil {
		return nil, err
	}
	if err = file.Sync(); err != nil {
		return nil, err
	}
	// Windows finalizes last-write time when the writable handle closes. Keep
	// a read handle to the same object so the returned version survives Close
	// and the subsequent commit still uses a pinned, verified staging file.
	if runtime.GOOS == "windows" {
		reader, err := openRegular(h, temp)
		if err != nil {
			return nil, err
		}
		staged, statErr := file.Stat()
		opened, openErr := reader.Stat()
		if statErr != nil || openErr != nil || !os.SameFile(staged, opened) {
			reader.Close()
			return nil, hosts.Fail("source_changed", "Staging file changed before commit")
		}
		closeErr := file.Close()
		file = reader
		if closeErr != nil {
			return nil, closeErr
		}
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	if _, _, err = f.check(ctx, call, p.Path, true); err != nil {
		return nil, err
	}
	if err = f.cfg.Check(ctx, call, abs, true); err != nil {
		return nil, err
	}
	current, err := h.Lstat(name)
	currentVersion := ""
	currentExists := err == nil
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	if currentExists {
		if !current.Mode().IsRegular() {
			return nil, hosts.Fail("version_conflict", "Destination type changed")
		}
		currentVersion = version(current)
	}
	if err = p.Condition.Check(currentVersion, currentExists); err != nil {
		return nil, err
	}
	// Atomic no-replace works without hard-link support (e.g. Windows FAT
	// volumes) and cannot overwrite a destination created after preflight.
	if p.Condition.Absent {
		dir, openErr := h.Open(".")
		if openErr != nil {
			return nil, openErr
		}
		err = renameNoReplace(int(dir.Fd()), temp, int(dir.Fd()), name)
		dir.Close()
	} else {
		err = h.Rename(temp, name)
	}
	if err != nil {
		return nil, err
	}
	// Cancellation after commit must not turn a completed save into a retry.
	info, err := file.Stat()
	if err != nil {
		e := hosts.Fail("filesystem_error", "File committed but metadata could not be read")
		e.Effect = "partial"
		return nil, e
	}
	e := entry(p.Path, info)
	e.Name = name
	return e, nil
}
func (f *FS) mkdir(ctx context.Context, call Call, p mkdirArgs) (any, error) {
	if p.Parents {
		return f.mkdirParents(ctx, call, p)
	}
	if len(p.Path.Segments) == 0 {
		return nil, hosts.Fail("permission_denied", "Cannot create a root")
	}
	r, abs, err := f.check(ctx, call, p.Path, true)
	if err != nil {
		return nil, err
	}
	h, name, err := parent(r, abs)
	if err != nil {
		return nil, err
	}
	defer h.Close()
	err = h.Mkdir(name, 0o700)
	if err != nil && !(p.ExistOK && errors.Is(err, fs.ErrExist)) {
		return nil, err
	}
	info, err := h.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, hosts.Fail("already_exists", "Destination is not a directory")
	}
	return entry(p.Path, info), nil
}
func (f *FS) remove(ctx context.Context, call Call, p removeArgs) (any, error) {
	if p.Recursive {
		return f.removeTree(ctx, call, p)
	}
	if len(p.Path.Segments) == 0 {
		return nil, hosts.Fail("permission_denied", "Cannot remove a root")
	}
	r, abs, err := f.check(ctx, call, p.Path, true)
	if err != nil {
		return nil, err
	}
	h, name, err := parent(r, abs)
	if err != nil {
		return nil, err
	}
	defer h.Close()
	info, err := h.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) && p.MissingOK {
		return map[string]any{"removed": false}, nil
	}
	if err != nil {
		return nil, err
	}
	if version(info) != p.IfVersion {
		return nil, hosts.Fail("version_conflict", "Destination changed before removal")
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	if err = h.Remove(name); err != nil {
		return nil, err
	}
	return map[string]any{"removed": true, "path": p.Path}, nil
}

// ResultPolicy binds a metadata snapshot to its originally checked paths. The
// verifier only calls the device policy, without reacquiring the provider lock:
// a consumer may legally use the immutable JSON source as fs.write input.
func (f *FS) ResultPolicy(call Call) (func(context.Context) error, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if call.Method == "roots" || call.Method == "home" {
		return func(context.Context) error { return nil }, nil
	}
	var p struct {
		Path fsp.Path `json:"path"`
		Src  fsp.Path `json:"src"`
		Dst  fsp.Path `json:"dst"`
	}
	if err := json.Unmarshal(call.Args, &p); err != nil {
		return nil, err
	}
	type target struct {
		path  string
		write bool
	}
	var paths []target
	add := func(path fsp.Path, write bool) error {
		r, abs, err := f.check(context.Background(), call, path, write)
		if err != nil {
			return err
		}
		requested := filepath.Join(append([]string{r.Path}, path.Segments...)...)
		paths = append(paths, target{requested, write}, target{abs, write})
		return nil
	}
	write := call.Method == "write" || call.Method == "mkdir" || call.Method == "remove" || call.Method == "move" || call.Method == "copy"
	if call.Method == "move" || call.Method == "copy" {
		if err := add(p.Src, call.Method == "move"); err != nil {
			return nil, err
		}
		p.Path = p.Dst
	}
	if err := add(p.Path, write); err != nil {
		return nil, err
	}
	return func(ctx context.Context) error {
		for _, p := range paths {
			if err := f.cfg.Check(ctx, call, p.path, p.write); err != nil {
				return err
			}
		}
		return nil
	}, nil
}

type fsMethod struct {
	InputSchema json.RawMessage
	Effect      string
}
type Call struct {
	Caller                 tool.Caller
	Owner, Command, Method string
	Args                   json.RawMessage
}

func Owner(c tool.Caller) string {
	h := sha256.Sum256([]byte(c.Subject + "\x00" + c.Origin))
	return hex.EncodeToString(h[:16])
}
func (f *FS) Run(ctx context.Context, call Call) (any, error) {
	if err := f.Authorize(ctx, call); err != nil {
		return nil, err
	}
	return f.run(ctx, call)
}

func (f *FS) Configure(home fsp.Path, proxyLimit int64) {
	if proxyLimit <= 0 {
		proxyLimit = 64 << 20
	}
	f.mu.Lock()
	f.cfg.Home = &home
	f.cfg.MaxProxyUploadBytes = proxyLimit
	f.mu.Unlock()
}
