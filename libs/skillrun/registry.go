package skillrun

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/veypi/vbox"
	"github.com/veypi/vsh/commands"

	vshglue "github.com/veypi/aic-pod/libs/vsh"
)

// Deps 是 Registry 的装配依赖（全部由 host 装配侧注入，skillrun 不 import
// cfg/host——保持与 provider 同向的依赖边界）。
type Deps struct {
	// SkillsDir 设备安装根（host = $HOME/.aic/skills）。
	SkillsDir string
	// RunDir service socket 目录（host = $HOME/.aic/run；macOS unix socket
	// 104 字符路径上限，不能深）。
	RunDir string
	// Manager 进程托管（vbox OS 沙箱 + 受管取消）。
	Manager *vbox.Manager
	// Policy 当次沙箱策略快照源（= host.nativePolicy：与 native 命令同一派生）。
	Policy func(ctx context.Context, workdir, name string) vshglue.NativePolicy
	// Workdir 引擎规范形 cwd → OS 原生态（= proto.HostPathToOS）。
	Workdir func(invCwd string) string
	// Registry vsh 引擎命令表（懒解析——引擎可能尚未构建；安装/卸载时取值）。
	Registry func() (*commands.Registry, error)
	// Tasks bg 任务表（service 懒启动登记；懒解析同 Registry）。nil = 不登记。
	Tasks func() (*vshglue.TaskTable, error)
	// Fetch 包拉取（host = NATS fetch subject 分块拉 zip；v6 P2）。
	// nil = download 不可用（Install 本地目录安装不受影响）。
	Fetch func(ctx context.Context, ref, version string) (zipData []byte, meta *FetchMeta, err error)
	// Logf 可选日志。
	Logf func(format string, args ...any)
}

// Package 是一个已安装包的运行态。
type Package struct {
	Name     string // 包名 = 根命令（安装目录 basename）
	Dir      string // 安装目录 {SkillsDir}/{name}
	Manifest *Manifest

	record   InstallRecord // .install.json（来源身份/禁用态的事实源）
	mu       sync.RWMutex
	disabled bool
}

// Disabled 报告包是否被禁用（禁用 = 根命令保留注册但显式失败）。
func (p *Package) Disabled() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.disabled
}

// Record 返回安装记录快照（.install.json 内容——来源身份/版本/禁用态）。
func (p *Package) Record() InstallRecord {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.record
}

// Registry 是包命令注册表生命周期的唯一权威（冲突检查/注册/禁用/卸载）。
type Registry struct {
	deps Deps
	mu   sync.Mutex
	pkgs map[string]*Package
	svcs map[string]*serviceInst
	seq  int64 // invoke/stream 帧 ID 序列
}

// namePattern 包名形态（保守：小写字母数字中划线——根命令直接进 shell 命名空间）。
var namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

func New(deps Deps) (*Registry, error) {
	if deps.SkillsDir == "" {
		return nil, fmt.Errorf("skillrun: SkillsDir required")
	}
	if deps.Manager == nil {
		return nil, fmt.Errorf("skillrun: Manager required")
	}
	if deps.Registry == nil {
		return nil, fmt.Errorf("skillrun: Registry source required")
	}
	if err := os.MkdirAll(deps.SkillsDir, 0o700); err != nil {
		return nil, fmt.Errorf("skillrun: skills dir: %w", err)
	}
	return &Registry{deps: deps, pkgs: map[string]*Package{}, svcs: map[string]*serviceInst{}}, nil
}

func (r *Registry) logf(format string, args ...any) {
	if r.deps.Logf != nil {
		r.deps.Logf(format, args...)
	}
}

// Install 从本地源目录安装（P0 内部 API；NATS download 的解包落盘阶段与
// 原子性序列在 P2 接线）：校验 manifest → 包名冲突全检 → 拷贝目录 → 补 entry
// 执行位 → RegisterGuarded 根命令。包名 = 源目录 basename。
func (r *Registry) Install(srcDir string) (*Package, error) {
	name := filepath.Base(filepath.Clean(srcDir))
	if !namePattern.MatchString(name) {
		return nil, fmt.Errorf("skillrun: invalid package name %q (want %s)", name, namePattern)
	}
	m, err := ParseManifest(filepath.Join(srcDir, "cli", "manifest.json"))
	if err != nil {
		return nil, err
	}
	reg, err := r.deps.Registry()
	if err != nil {
		return nil, fmt.Errorf("skillrun: engine registry: %w", err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	// 来源裁决（v6 P2 与 Download 同语义）：同源同名 = 更新；异源同名 =
	// 显式报错不覆盖。本地目录安装的来源 = kind:private id:包名（v6.1：
	// local 更名 private——云端统一私有/公开二分）。
	rec := InstallRecord{Name: name, Kind: "private", ID: name, InstalledAt: time.Now().UTC()}
	if old, exists := r.pkgs[name]; exists {
		if old.record.Kind != rec.Kind || old.record.ID != rec.ID {
			return nil, fmt.Errorf("skillrun: %q 已安装且来源不同（%s:%s）——先卸载再安装", name, old.record.Kind, old.record.ID)
		}
		rec.Disabled = old.record.Disabled
	} else if _, taken := reg.Lookup(name); taken {
		// 包名冲突全检（仅新装）：vs 内建/保留名/已装包（vsh 注册表一次
		// Lookup 全覆盖——内建与已注册包命令都在表里）。更新路径的旧根
		// 命令在下方 Unregister 后才让位，不能在这里拒。
		return nil, fmt.Errorf("skillrun: command %q already registered（包名冲突，显式拒绝）", name)
	}
	// 停旧 provider + 解注册（更新路径；幂等）。
	r.killServicesLocked(name)
	reg.Unregister(name)
	// 拷贝目录 → 补 entry 执行位。
	dst := filepath.Join(r.deps.SkillsDir, name)
	if err := os.RemoveAll(dst); err != nil {
		return nil, fmt.Errorf("skillrun: clear target: %w", err)
	}
	if err := copyDir(srcDir, dst); err != nil {
		return nil, fmt.Errorf("skillrun: copy: %w", err)
	}
	pkg := &Package{Name: name, Dir: dst, Manifest: m}
	for _, p := range m.Providers {
		entry := filepath.Join(dst, filepath.FromSlash(p.Entry))
		if st, err := os.Stat(entry); err != nil {
			return nil, fmt.Errorf("skillrun: provider %q entry: %w", p.ID, err)
		} else if st.IsDir() {
			return nil, fmt.Errorf("skillrun: provider %q entry is a directory", p.ID)
		}
		if err := os.Chmod(entry, 0o755); err != nil {
			return nil, fmt.Errorf("skillrun: provider %q entry exec bit: %w", p.ID, err)
		}
	}
	// 写 .install.json = 提交标记（启动扫描重注册的事实源）。
	if err := writeRecord(dst, rec); err != nil {
		return nil, fmt.Errorf("skillrun: write install record: %w", err)
	}
	pkg.disabled = rec.Disabled
	// 注册根命令（RegisterGuarded 兜底——冲突全检与注册之间的并发由 r.mu +
	// 注册表错误双保险）。
	if err := reg.RegisterGuarded(r.rootCommand(pkg)); err != nil {
		return nil, fmt.Errorf("skillrun: register root command: %w", err)
	}
	pkg.record = rec
	r.pkgs[name] = pkg
	r.logf("skillrun: installed %s -> %s", name, dst)
	return pkg, nil
}

// Uninstall 卸载：解注册 + bg kill 驻留 provider + 删目录。幂等。
func (r *Registry) Uninstall(name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	pkg, exists := r.pkgs[name]
	if !exists {
		return nil
	}
	reg, err := r.deps.Registry()
	if err != nil {
		return fmt.Errorf("skillrun: engine registry: %w", err)
	}
	reg.Unregister(name)
	r.killServicesLocked(name)
	delete(r.pkgs, name)
	if err := os.RemoveAll(pkg.Dir); err != nil {
		return fmt.Errorf("skillrun: remove %s: %w", pkg.Dir, err)
	}
	r.logf("skillrun: uninstalled %s", name)
	return nil
}

// SetDisabled 禁用/启用：禁用 = 根命令保留注册但调用显式失败（防回落同名
// 系统程序），不删目录不解注册；禁用态落 .install.json 持久（重启恢复）。
func (r *Registry) SetDisabled(name string, disabled bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	pkg, exists := r.pkgs[name]
	if !exists {
		return fmt.Errorf("skillrun: package %q not installed", name)
	}
	pkg.mu.Lock()
	pkg.disabled = disabled
	pkg.record.Disabled = disabled
	rec := pkg.record
	pkg.mu.Unlock()
	if err := writeRecord(pkg.Dir, rec); err != nil {
		return fmt.Errorf("skillrun: persist disabled state: %w", err)
	}
	return nil
}

// IsPackageCommand 报告 name 是否为已装包的根命令（CommandAllow 泛化查询：
// 包命令与 browser/cua 同走 execAllowed 门径）。
func (r *Registry) IsPackageCommand(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, exists := r.pkgs[name]
	return exists
}

// Get 返回已装包（未装 = nil）。
func (r *Registry) Get(name string) *Package {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.pkgs[name]
}

// List 返回全部已装包名（排序由调用方需要时自行处理）。
func (r *Registry) List() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.pkgs))
	for name := range r.pkgs {
		out = append(out, name)
	}
	return out
}

// ResolveStreamEndpoint 解析 RTC stream 端点名 →（包名，流名）（v6 P5 泛化，
// OpenToolStream 唯一解析路径）：
//  1. {包名}.{流名} 直查——包已安装且 manifest streams[] 含该流名；
//  2. 全端点名 = 流名——包 manifest 把 StreamDecl.Name 声明为带点全名（如
//     browser 包的 page.frames/page.input：端点名保留 v5 前端契约，不随包名
//     变化）。
func (r *Registry) ResolveStreamEndpoint(endpoint string) (pkgName, streamName string, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	hasStream := func(pkg *Package, name string) bool {
		if pkg.Manifest == nil {
			return false
		}
		for _, s := range pkg.Manifest.Streams {
			if s.Name == name {
				return true
			}
		}
		return false
	}
	if pkg, stream, found := strings.Cut(endpoint, "."); found && stream != "" && !strings.Contains(stream, ".") {
		if p, exists := r.pkgs[pkg]; exists && hasStream(p, stream) {
			return pkg, stream, true
		}
	}
	for name, p := range r.pkgs {
		if hasStream(p, endpoint) {
			return name, endpoint, true
		}
	}
	return "", "", false
}

// copyDir 递归拷贝目录（保持权限位；symlink 解引用为实体——包内容必须自洽，
// 不许借 symlink 把 entry 指到包外）。
func copyDir(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("non-regular file in package: %s", rel)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, info.Mode().Perm())
	})
}
