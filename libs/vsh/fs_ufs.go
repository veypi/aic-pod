package vsh

import (
	"context"
	"errors"
	"fmt"
	"io"
	stdfs "io/fs"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/veypi/vbox"
	"github.com/veypi/vigo/contrib/ufs"
	gbfs "github.com/veypi/vsh/fs"
)

// ErrRuleDenied 规则表拒绝（报错文案引导 grant——v4 正交化：不审批、硬拒）。
var ErrRuleDenied = errors.New("path denied by rule table")

// ErrOutsideJail 用户根 jail 硬约束拒绝（cloud：/u/{uid} 之外无配置通道）。
var ErrOutsideJail = errors.New("path outside user root (jail)")

// ErrUnsupportedOp UFS 不支持的操作（symlink/hardlink/chmod/chown——UFS 无
// 符号链接与权限位 API；验收 8：可执行位不持久化，工具描述引导 bash x.sh）。
var ErrUnsupportedOp = errors.New("operation unsupported on UFS backing")

// UFSAdapterConfig 是 ufsAdapter 的装配参数（cloud 与 host 共用同一适配器
// 核心：ufs.FS backing + vbox 规则表门 + 可选 jail + 可选内存层前缀）。
type UFSAdapterConfig struct {
	// Backing UFS 直通（cloud 调用方已包 QuotaFS——配额闸门在 backing 层）。
	Backing ufs.FS
	// Rules 规则表快照源：每次 IO 取当次值（grant temp 动态行即时生效）。
	// nil = 不挂规则表（仅测试）。
	Rules func() vbox.FSRuleSet
	// JailRoot 用户根 jail（cloud = /u/{uid}）：代码硬约束，规则表管不到
	// 界外——界外路径即使写入规则行也不生效。空 = 不启用（host）。
	JailRoot string
	// MemPrefixes 内存层前缀集（cloud = /bin /usr/bin /tmp /etc /dev /proc）：
	// per-session 内存层，用完即弃，UFS 零污染（红线）。
	MemPrefixes []string
	// SeedMem 内存层初始文件（stub）；键为绝对路径。
	SeedMem map[string][]byte
}

// ufsAdapter 把 ufs.FS 适配为引擎 gbfs.FileSystem，并在进程内执行
// canonicalize-then-check 的 vbox 规则表门（唯一进程内路径权威）。
type ufsAdapter struct {
	backing   ufs.FS
	rules     func() vbox.FSRuleSet
	jail      string
	mem       *gbfs.MemoryFS
	memPrefix []string
	mu        sync.Mutex
	cwd       string
}

// NewUFSAdapter 构造适配器（mem 层种子写入失败即报错——stub 落位是启动契约）。
func NewUFSAdapter(cfg UFSAdapterConfig) (gbfs.FileSystem, error) {
	if cfg.Backing == nil {
		return nil, fmt.Errorf("vsh glue: ufs backing required")
	}
	a := &ufsAdapter{
		backing:   cfg.Backing,
		rules:     cfg.Rules,
		jail:      strings.TrimSuffix(cfg.JailRoot, "/"),
		mem:       gbfs.NewMemory(),
		memPrefix: cfg.MemPrefixes,
		cwd:       "/",
	}
	if a.jail != "" {
		a.cwd = a.jail
	}
	for name, data := range cfg.SeedMem {
		f, err := a.mem.OpenFile(context.Background(), gbfs.Clean(name), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
		if err != nil {
			return nil, fmt.Errorf("vsh glue: seed mem %s: %w", name, err)
		}
		if _, err := f.Write(data); err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("vsh glue: seed mem %s: %w", name, err)
		}
		_ = f.Close()
	}
	return a, nil
}

// CloudFSConfig cloud 会话文件系统参数（design §4.1）。
type CloudFSConfig struct {
	// UserRoot = /u/{uid}（HOME；jail 硬约束根）。
	UserRoot string
	// Backing UFS 直通（调用方已包 QuotaFS）。
	Backing ufs.FS
	// Rules vbox 规则表快照源（行序：temp → 便利根 rw 会话目录 → ro 行；
	// DefaultWrite deny）。
	Rules func() vbox.FSRuleSet
	// Stubs 内存层 stub 文件（/bin、/usr/bin 下；PATH 钉死 /usr/bin:/bin）。
	Stubs map[string][]byte
}

// NewCloudFS cloud：UFS 直通 + 系统目录内存层 + 用户根 jail + vbox 规则表门。
// 用户根在此确保存在（构造期直写 backing，不经规则表门——平台初始化动作；
// 引擎 init 的 MkdirAll(HOME/workDir) 走门且依赖已存在的根）。
func NewCloudFS(cfg CloudFSConfig) (gbfs.FileSystem, error) {
	if cfg.UserRoot == "" {
		return nil, fmt.Errorf("vsh glue: cloud user root required")
	}
	if cfg.Backing == nil {
		return nil, fmt.Errorf("vsh glue: ufs backing required")
	}
	if err := cfg.Backing.MkdirAll(cfg.UserRoot, 0o755); err != nil {
		return nil, fmt.Errorf("vsh glue: init user root: %w", err)
	}
	return NewUFSAdapter(UFSAdapterConfig{
		Backing:     cfg.Backing,
		Rules:       cfg.Rules,
		JailRoot:    cfg.UserRoot,
		MemPrefixes: []string{"/bin", "/usr/bin", "/tmp", "/etc", "/dev", "/proc"},
		SeedMem:     cfg.Stubs,
	})
}

// resolve 把引擎传入路径（可相对）归一为绝对逻辑路径（posix 语义）。
func (a *ufsAdapter) resolve(name string) string {
	if name == "" {
		a.mu.Lock()
		defer a.mu.Unlock()
		return a.cwd
	}
	if !strings.HasPrefix(name, "/") {
		a.mu.Lock()
		name = a.cwd + "/" + name
		a.mu.Unlock()
	}
	return gbfs.Clean(name)
}

// isMem 判定路径是否路由进内存层。
func (a *ufsAdapter) isMem(abs string) bool {
	for _, p := range a.memPrefix {
		if abs == p || strings.HasPrefix(abs, p+"/") {
			return true
		}
	}
	return false
}

// fileOpName 是 vbox.FileOp 的日志/错误文案（vbox 未暴露 String）。
func fileOpName(op vbox.FileOp) string {
	if op == vbox.OpWrite {
		return "write"
	}
	return "read"
}

// gate 进程内路径权威：jail 硬约束 → vbox 规则表（首命中生效）。
// noFollow = unlink/rename 语义（作用于链接本身，不跟随末段）。
func (a *ufsAdapter) gate(abs string, op vbox.FileOp, noFollow bool) error {
	if a.jail != "" && abs != a.jail && !strings.HasPrefix(abs, a.jail+"/") {
		return &stdfs.PathError{Op: fileOpName(op), Path: abs, Err: fmt.Errorf("%w: %s（cloud 文件访问限定在 %s 之下）", ErrOutsideJail, abs, a.jail)}
	}
	if a.rules == nil {
		return nil
	}
	rules := a.rules()
	var d vbox.Decision
	if noFollow {
		d = rules.MatchNoFollow(abs, op)
	} else {
		d = rules.Match(abs, op)
	}
	if !d.Allow {
		return &stdfs.PathError{Op: fileOpName(op), Path: abs, Err: fmt.Errorf("%w: %s（越界硬拒绝；如需访问请 grant fs %s）", ErrRuleDenied, abs, abs)}
	}
	return nil
}

// --- gbfs.FileSystem 实现 ---

func (a *ufsAdapter) Open(ctx context.Context, name string) (gbfs.File, error) {
	abs := a.resolve(name)
	if a.isMem(abs) {
		return a.mem.Open(ctx, abs)
	}
	if err := a.gate(abs, vbox.OpRead, false); err != nil {
		return nil, err
	}
	f, err := a.backing.Open(abs)
	if err != nil {
		return nil, err
	}
	return roFile{File: f, name: abs}, nil
}

func (a *ufsAdapter) OpenFile(ctx context.Context, name string, flag int, perm stdfs.FileMode) (gbfs.File, error) {
	abs := a.resolve(name)
	if a.isMem(abs) {
		return a.mem.OpenFile(ctx, abs, flag, perm)
	}
	writeIntent := flag&(os.O_WRONLY|os.O_RDWR|os.O_CREATE|os.O_TRUNC|os.O_APPEND) != 0
	op := vbox.OpRead
	if writeIntent {
		op = vbox.OpWrite
	}
	if err := a.gate(abs, op, false); err != nil {
		return nil, err
	}
	if !writeIntent {
		f, err := a.backing.Open(abs)
		if err != nil {
			return nil, err
		}
		return roFile{File: f, name: abs}, nil
	}
	switch {
	case flag&os.O_APPEND != 0 || flag&(os.O_CREATE|os.O_TRUNC) == 0:
		// append / 纯 O_WRONLY 已存在文件：ufs.FS 无 OpenFile 旗帜，backing.Open
		// 多为只读句柄（localFS = os.Open）——读改写回退：装载现有内容，
		// 内存缓冲写，Close 时 WriteFile 整写回（shell 追加/改写语义足够；
		// 并发 append 原子性不保证，记录在案）。
		return newBufferedWriteFile(a.backing, abs, flag&os.O_APPEND != 0)
	case flag&(os.O_CREATE|os.O_TRUNC) != 0:
		// Create = 创建或截断（ufs 语义），覆盖 O_CREATE|O_TRUNC 组合。
		return a.backing.Create(abs)
	default:
		return nil, &stdfs.PathError{Op: "open", Path: abs, Err: ErrUnsupportedOp}
	}
}

func (a *ufsAdapter) Stat(ctx context.Context, name string) (stdfs.FileInfo, error) {
	abs := a.resolve(name)
	if a.isMem(abs) {
		return a.mem.Stat(ctx, abs)
	}
	if err := a.gate(abs, vbox.OpRead, false); err != nil {
		return nil, err
	}
	return a.backing.Stat(abs)
}

func (a *ufsAdapter) Lstat(ctx context.Context, name string) (stdfs.FileInfo, error) {
	abs := a.resolve(name)
	if a.isMem(abs) {
		return a.mem.Lstat(ctx, abs)
	}
	// UFS 无符号链接：Lstat = Stat（但门控用 NoFollow 口径——查的是路径本身）。
	if err := a.gate(abs, vbox.OpRead, true); err != nil {
		return nil, err
	}
	return a.backing.Stat(abs)
}

func (a *ufsAdapter) ReadDir(ctx context.Context, name string) ([]stdfs.DirEntry, error) {
	abs := a.resolve(name)
	if a.isMem(abs) {
		return a.mem.ReadDir(ctx, abs)
	}
	if err := a.gate(abs, vbox.OpRead, false); err != nil {
		return nil, err
	}
	return a.backing.ReadDir(abs)
}

func (a *ufsAdapter) Readlink(ctx context.Context, name string) (string, error) {
	abs := a.resolve(name)
	if a.isMem(abs) {
		return a.mem.Readlink(ctx, abs)
	}
	return "", &stdfs.PathError{Op: "readlink", Path: abs, Err: ErrUnsupportedOp}
}

func (a *ufsAdapter) Realpath(ctx context.Context, name string) (string, error) {
	abs := a.resolve(name)
	if a.isMem(abs) {
		return a.mem.Realpath(ctx, abs)
	}
	// UFS 无符号链接：realpath = 词法归一。
	if err := a.gate(abs, vbox.OpRead, false); err != nil {
		return "", err
	}
	return abs, nil
}

func (a *ufsAdapter) Symlink(ctx context.Context, target, linkName string) error {
	abs := a.resolve(linkName)
	if a.isMem(abs) {
		return a.mem.Symlink(ctx, target, abs)
	}
	return &stdfs.PathError{Op: "symlink", Path: abs, Err: ErrUnsupportedOp}
}

func (a *ufsAdapter) Link(ctx context.Context, oldName, newName string) error {
	abs := a.resolve(newName)
	if a.isMem(abs) {
		return a.mem.Link(ctx, a.resolve(oldName), abs)
	}
	return &stdfs.PathError{Op: "link", Path: abs, Err: ErrUnsupportedOp}
}

func (a *ufsAdapter) Chmod(ctx context.Context, name string, mode stdfs.FileMode) error {
	abs := a.resolve(name)
	if a.isMem(abs) {
		return a.mem.Chmod(ctx, abs, mode)
	}
	// UFS 无 chmod API（验收 8：可执行位不持久化 → 工具描述引导 bash x.sh）。
	if err := a.gate(abs, vbox.OpWrite, false); err != nil {
		return err
	}
	return &stdfs.PathError{Op: "chmod", Path: abs, Err: ErrUnsupportedOp}
}

func (a *ufsAdapter) Chown(ctx context.Context, name string, uid, gid uint32, follow bool) error {
	abs := a.resolve(name)
	if a.isMem(abs) {
		return a.mem.Chown(ctx, abs, uid, gid, follow)
	}
	return &stdfs.PathError{Op: "chown", Path: abs, Err: ErrUnsupportedOp}
}

func (a *ufsAdapter) Chtimes(ctx context.Context, name string, atime, mtime time.Time) error {
	abs := a.resolve(name)
	if a.isMem(abs) {
		return a.mem.Chtimes(ctx, abs, atime, mtime)
	}
	if err := a.gate(abs, vbox.OpWrite, false); err != nil {
		return err
	}
	// ufs.FS 无 Chtimes：backing 实现支持则用，否则尽力而为（touch/cp -p/tar
	// 不因此失败；mtime 语义降级，记录在案）。
	if ct, ok := a.backing.(interface {
		Chtimes(name string, atime, mtime time.Time) error
	}); ok {
		return ct.Chtimes(abs, atime, mtime)
	}
	return nil
}

func (a *ufsAdapter) Lchtimes(ctx context.Context, name string, atime, mtime time.Time) error {
	abs := a.resolve(name)
	if a.isMem(abs) {
		return a.mem.Lchtimes(ctx, abs, atime, mtime)
	}
	return a.Chtimes(ctx, abs, atime, mtime)
}

func (a *ufsAdapter) MkdirAll(ctx context.Context, name string, perm stdfs.FileMode) error {
	abs := a.resolve(name)
	if a.isMem(abs) {
		return a.mem.MkdirAll(ctx, abs, perm)
	}
	// 幂等短路（对齐 os.MkdirAll 语义）：已存在的目录直接返回 nil——引擎 init
	// 会 MkdirAll(workingDir)，根/会话目录多已存在，不该被写门拦下。
	// 存在性检查不泄露内容，与读门分开记录。
	if fi, err := a.backing.Stat(abs); err == nil && fi.IsDir() {
		return nil
	}
	if err := a.gate(abs, vbox.OpWrite, false); err != nil {
		return err
	}
	return a.backing.MkdirAll(abs, perm)
}

func (a *ufsAdapter) Mkfifo(ctx context.Context, name string, perm stdfs.FileMode) error {
	abs := a.resolve(name)
	if a.isMem(abs) {
		return a.mem.Mkfifo(ctx, abs, perm)
	}
	return &stdfs.PathError{Op: "mkfifo", Path: abs, Err: ErrUnsupportedOp}
}

func (a *ufsAdapter) Remove(ctx context.Context, name string, recursive bool) error {
	abs := a.resolve(name)
	if a.isMem(abs) {
		return a.mem.Remove(ctx, abs, recursive)
	}
	// unlink 语义：删的是路径本身（NoFollow，2026-09-22 .venv 修复同口径）。
	if err := a.gate(abs, vbox.OpWrite, true); err != nil {
		return err
	}
	if !recursive {
		// rmdir 语义：非空目录不删（RemoveAll 无此区分，在此补齐）。
		if fi, err := a.backing.Stat(abs); err == nil && fi.IsDir() {
			if entries, rerr := a.backing.ReadDir(abs); rerr == nil && len(entries) > 0 {
				return &stdfs.PathError{Op: "remove", Path: abs, Err: errors.New("directory not empty")}
			}
		}
	}
	return a.backing.RemoveAll(abs)
}

func (a *ufsAdapter) Rename(ctx context.Context, oldName, newName string) error {
	oldAbs, newAbs := a.resolve(oldName), a.resolve(newName)
	oldMem, newMem := a.isMem(oldAbs), a.isMem(newAbs)
	if oldMem && newMem {
		return a.mem.Rename(ctx, oldAbs, newAbs)
	}
	if oldMem != newMem {
		// 跨层 rename 不支持（内存层 per-session 即弃；引擎 cp 走读写复制）。
		return &stdfs.PathError{Op: "rename", Path: oldAbs + " -> " + newAbs, Err: errors.New("cross-layer rename unsupported")}
	}
	// rename 两端都判写（NoFollow——作用于路径本身）。
	if err := a.gate(oldAbs, vbox.OpWrite, true); err != nil {
		return err
	}
	if err := a.gate(newAbs, vbox.OpWrite, true); err != nil {
		return err
	}
	return a.backing.Rename(oldAbs, newAbs)
}

func (a *ufsAdapter) Getwd() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cwd
}

func (a *ufsAdapter) Chdir(name string) error {
	abs := a.resolve(name)
	ctx := context.Background()
	var fi stdfs.FileInfo
	var err error
	if a.isMem(abs) {
		fi, err = a.mem.Stat(ctx, abs)
	} else {
		if gerr := a.gate(abs, vbox.OpRead, false); gerr != nil {
			return gerr
		}
		fi, err = a.backing.Stat(abs)
	}
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return &stdfs.PathError{Op: "chdir", Path: abs, Err: errors.New("not a directory")}
	}
	a.mu.Lock()
	a.cwd = abs
	a.mu.Unlock()
	return nil
}

// --- gbfs.File 适配 ---

// roFile 包装只读 fs.File 为 gbfs.File（Write 拒绝）。
type roFile struct {
	stdfs.File
	name string
}

func (f roFile) Write([]byte) (int, error) {
	return 0, &stdfs.PathError{Op: "write", Path: f.name, Err: stdfs.ErrPermission}
}

// rwSeekCloser 保留接口（backing.Open 可写断言目标，预留给支持 OpenFile 的
// backing；当前 localFS 只读句柄走 bufferedWriteFile 回退）。
type rwSeekCloser interface {
	io.Reader
	io.Writer
	io.Seeker
	io.Closer
}

// bufferedWriteFile 读改写回退文件：装载现有内容到内存缓冲，Write 按光标
// 覆盖/追加，Close 时 WriteFile 整写回 backing。
type bufferedWriteFile struct {
	backing ufs.FS
	name    string
	buf     []byte
	off     int64
	closed  bool
}

func newBufferedWriteFile(backing ufs.FS, name string, appendMode bool) (gbfs.File, error) {
	f := &bufferedWriteFile{backing: backing, name: name}
	data, err := backing.ReadFile(name)
	if err != nil {
		if !errors.Is(err, stdfs.ErrNotExist) {
			return nil, err
		}
		data = nil
	}
	f.buf = append([]byte(nil), data...)
	if appendMode {
		f.off = int64(len(f.buf))
	}
	return f, nil
}

func (f *bufferedWriteFile) Read(p []byte) (int, error) {
	if f.off >= int64(len(f.buf)) {
		return 0, io.EOF
	}
	n := copy(p, f.buf[f.off:])
	f.off += int64(n)
	return n, nil
}

func (f *bufferedWriteFile) Write(p []byte) (int, error) {
	if f.closed {
		return 0, &stdfs.PathError{Op: "write", Path: f.name, Err: stdfs.ErrClosed}
	}
	end := f.off + int64(len(p))
	if end > int64(len(f.buf)) {
		grown := make([]byte, end)
		copy(grown, f.buf)
		f.buf = grown
	}
	copy(f.buf[f.off:], p)
	f.off = end
	return len(p), nil
}

func (f *bufferedWriteFile) Close() error {
	if f.closed {
		return nil
	}
	f.closed = true
	return f.backing.WriteFile(f.name, f.buf, 0o644)
}

func (f *bufferedWriteFile) Stat() (stdfs.FileInfo, error) {
	if fi, err := f.backing.Stat(f.name); err == nil {
		return fi, nil
	}
	// 新文件尚未落盘：合成最小 FileInfo。
	return synthFileInfo{name: f.name, size: int64(len(f.buf))}, nil
}

// synthFileInfo 未落盘文件的最小 FileInfo。
type synthFileInfo struct {
	name string
	size int64
}

func (s synthFileInfo) Name() string {
	if idx := strings.LastIndex(s.name, "/"); idx >= 0 {
		return s.name[idx+1:]
	}
	return s.name
}
func (s synthFileInfo) Size() int64          { return s.size }
func (s synthFileInfo) Mode() stdfs.FileMode { return 0o644 }
func (s synthFileInfo) ModTime() time.Time   { return time.Now() }
func (s synthFileInfo) IsDir() bool          { return false }
func (s synthFileInfo) Sys() any             { return nil }

// 接口断言：适配器实现引擎 FS 契约。
var _ gbfs.FileSystem = (*ufsAdapter)(nil)
