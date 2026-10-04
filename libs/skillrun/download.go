package skillrun

// Installation prepares and validates .next before stopping the old service.
// Same-source updates switch directories via .old; failure preserves the old package.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/veypi/vsh/commands"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// installRecordFile 安装记录文件名（包目录内；最后写 = 提交标记）。
const installRecordFile = ".install.json"

// InstallRecord 安装记录（.install.json）。skill list --json 与启动扫描的
// 数据源；~/.aic 会话可读（仅 config.yaml 单独 deny），fs 读面可辅查。
type InstallRecord struct {
	Name        string    `json:"name"`
	Kind        string    `json:"kind"` // private | public | builtin
	ID          string    `json:"id"`   // 来源 id（本地目录名 / 注册表 uuid / builtin 包名）
	Version     string    `json:"version,omitempty"`
	InstalledAt time.Time `json:"installed_at"`
	Disabled    bool      `json:"disabled,omitempty"`
}

// FetchMeta 包身份（fetch 应答的适配形态；host 侧由 proto.FetchResult 转换。
// builtin 首跑预装同用：Kind=builtin, ID=包名，Name/Version 取自 zip 内 SKILL.md）。
type FetchMeta struct {
	Name    string
	Kind    string // private | public | builtin
	ID      string
	Version string
}

// Download 经 NATS fetch 下载并原子安装。ref = 注册表 uuid 或 caller 私有行 name。
func (r *Registry) Download(ctx context.Context, ref, version string) (*Package, error) {
	if r.deps.Fetch == nil {
		return nil, fmt.Errorf("skillrun: 此端未接包获取通道")
	}
	zipData, meta, err := r.deps.Fetch(ctx, ref, version)
	if err != nil {
		return nil, err
	}
	return r.InstallZip(ctx, zipData, meta)
}

// InstallZip is the only installation path, including embedded packages.
// Staging never changes the running package. Mutations serialize per name.
func (r *Registry) InstallZip(ctx context.Context, data []byte, meta *FetchMeta) (*Package, error) {
	if meta == nil || !namePattern.MatchString(meta.Name) || meta.ID == "" || (meta.Kind != "private" && meta.Kind != "public" && meta.Kind != "builtin") {
		return nil, fmt.Errorf("skill install: invalid package identity")
	}
	files, err := readZipEntries(data)
	if err != nil {
		return nil, err
	}
	manifest, lock, err := validateZipCLI(files)
	if err != nil {
		return nil, err
	}
	unlock := r.lockPackage(meta.Name)
	defer unlock()
	r.mu.Lock()
	closed := r.closed
	r.mu.Unlock()
	if closed {
		return nil, fmt.Errorf("skill registry closed")
	}
	old := r.Get(meta.Name)
	rec := InstallRecord{Name: meta.Name, Kind: meta.Kind, ID: meta.ID, Version: meta.Version, InstalledAt: time.Now().UTC()}
	if old != nil {
		previous := old.Record()
		if previous.Kind != meta.Kind || previous.ID != meta.ID {
			return nil, fmt.Errorf("skill %q already installed from different source", meta.Name)
		}
		rec.Disabled = previous.Disabled
	}
	reg := r.deps.Registry
	if manifest != nil {
		if commands.IsShellBuiltin(meta.Name) {
			return nil, fmt.Errorf("command %q is a shell builtin", meta.Name)
		}
		if _, taken := reg.Lookup(meta.Name); taken && (old == nil || old.Manifest == nil) {
			return nil, fmt.Errorf("command %q already registered", meta.Name)
		}
	}
	final := filepath.Join(r.deps.SkillsDir, meta.Name)
	next := final + ".next"
	backup := final + ".old"
	if err := os.RemoveAll(next); err != nil {
		return nil, err
	}
	defer os.RemoveAll(next)
	if err := extractZipEntries(files, next); err != nil {
		return nil, err
	}
	if lock != nil {
		if err := r.fetchArtifacts(ctx, lock, next); err != nil {
			return nil, err
		}
	}
	if manifest != nil {
		entry, err := resolveEntry(next, manifest.Entry)
		if err != nil {
			return nil, fmt.Errorf("skill entry: %w", err)
		}
		info, err := os.Lstat(entry)
		if err != nil {
			return nil, fmt.Errorf("skill entry: %w", err)
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("skill entry must be a regular file")
		}
		if err := os.Chmod(entry, 0755); err != nil {
			return nil, err
		}
	}
	if err := writeRecord(next, rec); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := r.prepareChange(old); err != nil {
		return nil, err
	}
	committed := false
	defer func() {
		if !committed {
			finishChange(old)
		}
	}()
	if old != nil {
		if err := r.stopService(old); err != nil {
			return nil, err
		}
	}
	backed := false
	if _, err := os.Stat(final); err == nil {
		if err := os.RemoveAll(backup); err != nil {
			return nil, err
		}
		if err := os.Rename(final, backup); err != nil {
			return nil, err
		}
		backed = true
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	rollback := func(cause error) (*Package, error) {
		if backed {
			if err := os.Rename(backup, final); err != nil {
				return nil, fmt.Errorf("%w; rollback failed, preserved %s: %v", cause, backup, err)
			}
		}
		return nil, cause
	}
	if err := os.Rename(next, final); err != nil {
		return rollback(err)
	}
	pkg := &Package{Name: meta.Name, Dir: final, Manifest: manifest, record: rec, disabled: rec.Disabled}
	// Register only when adding CLI capability. Existing root handles resolve by name.
	if manifest != nil && (old == nil || old.Manifest == nil) {
		if err := reg.RegisterGuarded(r.rootCommand(meta.Name)); err != nil {
			if removeErr := os.RemoveAll(final); removeErr != nil {
				return nil, fmt.Errorf("%w; preserve backup %s: %v", err, backup, removeErr)
			}
			return rollback(err)
		}
	}
	r.mu.Lock()
	r.pkgs[meta.Name] = pkg
	r.mu.Unlock()
	if manifest == nil && old != nil && old.Manifest != nil {
		reg.Unregister(meta.Name)
	}
	if backed {
		if err := os.RemoveAll(backup); err != nil {
			r.logf("skill %s: backup cleanup: %v", meta.Name, err)
		}
	}
	committed = true
	return pkg, nil
}

// fetchArtifacts artifacts.lock 设备侧下载 + sha256 校验（不经平台中转）。
func (r *Registry) fetchArtifacts(ctx context.Context, lock *ArtifactsLock, pkgDir string) error {
	client := &http.Client{Timeout: 10 * time.Minute}
	for _, a := range lock.Artifacts {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.URL, nil)
		if err != nil {
			return err
		}
		resp, err := client.Do(req)
		if err != nil {
			return fmt.Errorf("artifact %s: %w", a.Path, err)
		}
		h := sha256.New()
		data, err := io.ReadAll(io.TeeReader(io.LimitReader(resp.Body, maxZipFileSize+1), h))
		resp.Body.Close()
		if err != nil {
			return fmt.Errorf("artifact %s: %w", a.Path, err)
		}
		if int64(len(data)) > maxZipFileSize {
			return fmt.Errorf("artifact %s exceeds size limit", a.Path)
		}
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("artifact %s: http %d", a.Path, resp.StatusCode)
		}
		if got := hex.EncodeToString(h.Sum(nil)); got != a.SHA256 {
			return fmt.Errorf("artifact %s: sha256 mismatch（got %s）", a.Path, got)
		}
		target := filepath.Join(pkgDir, filepath.FromSlash(a.Path))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(target, data, 0o755); err != nil {
			return err
		}
		r.logf("skillrun: artifact %s (%d bytes, sha256 ok)", a.Path, len(data))
	}
	return nil
}

// writeRecord 原子写 .install.json（tmp + rename）。
func writeRecord(pkgDir string, rec InstallRecord) error {
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	p := filepath.Join(pkgDir, installRecordFile)
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// readRecord 读 .install.json（无效 = nil——半包/提交失败的判定依据）。
func readRecord(pkgDir string) *InstallRecord {
	data, err := os.ReadFile(filepath.Join(pkgDir, installRecordFile))
	if err != nil {
		return nil
	}
	var rec InstallRecord
	if err := json.Unmarshal(data, &rec); err != nil || !namePattern.MatchString(rec.Name) || (rec.Kind != "private" && rec.Kind != "public" && rec.Kind != "builtin") || rec.ID == "" {
		return nil
	}
	return &rec
}

// recoverPackage handles only .next/.old left by the rename sequence.
func (r *Registry) recoverPackage(name string) error {
	final := filepath.Join(r.deps.SkillsDir, name)
	if _, err := os.Stat(final); err == nil {
		if _, err := loadInstalled(final, name); err != nil {
			return fmt.Errorf("invalid installed package %s: %w", name, err)
		}
		if err := os.RemoveAll(final + ".old"); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	} else if _, err := os.Stat(final + ".old"); err == nil {
		if _, err := loadInstalled(final+".old", name); err != nil {
			return fmt.Errorf("invalid backup %s: %w", name, err)
		}
		if err := os.Rename(final+".old", final); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	return os.RemoveAll(final + ".next")
}
func loadInstalled(dir, name string) (*Package, error) {
	rec := readRecord(dir)
	if rec == nil || rec.Name != name {
		return nil, fmt.Errorf("invalid install record")
	}
	var manifest *Manifest
	cli := filepath.Join(dir, "cli")
	if _, err := os.Stat(cli); err == nil {
		manifest, err = ParseManifest(filepath.Join(cli, "manifest.json"))
		if err != nil {
			return nil, err
		}
		entry, err := resolveEntry(dir, manifest.Entry)
		if err != nil {
			return nil, err
		}
		info, err := os.Lstat(entry)
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("entry is not a regular file")
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	return &Package{Name: name, Dir: dir, Manifest: manifest, record: *rec, disabled: rec.Disabled}, nil
}

// Rescan runs at startup before serving requests. Invalid packages stay unregistered.
func (r *Registry) Rescan() {
	entries, err := os.ReadDir(r.deps.SkillsDir)
	if err != nil {
		r.logf("skill rescan: %v", err)
		return
	}
	names := map[string]bool{}
	for _, entry := range entries {
		if entry.IsDir() {
			name := strings.TrimSuffix(strings.TrimSuffix(entry.Name(), ".next"), ".old")
			if namePattern.MatchString(name) {
				names[name] = true
			}
		}
	}
	for name := range names {
		unlock := r.lockPackage(name)
		if err := r.recoverPackage(name); err != nil {
			r.logf("skill %s recovery: %v", name, err)
			unlock()
			continue
		}
		pkg, err := loadInstalled(filepath.Join(r.deps.SkillsDir, name), name)
		if err == nil && pkg.Manifest != nil {
			if commands.IsShellBuiltin(name) {
				err = fmt.Errorf("shell builtin collision")
			} else {
				err = r.deps.Registry.RegisterGuarded(r.rootCommand(name))
			}
		}
		if err != nil {
			r.logf("skill %s skipped: %v", name, err)
		} else {
			r.mu.Lock()
			r.pkgs[name] = pkg
			r.mu.Unlock()
		}
		unlock()
	}
}

// Records 全部安装记录（skill list --json 数据源；按包名排序）。
func (r *Registry) Records() []InstallRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]InstallRecord, 0, len(r.pkgs))
	for _, pkg := range r.pkgs {
		out = append(out, pkg.Record())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// RecordsJSON 安装记录 JSON（skill list --json 输出）。
func (r *Registry) RecordsJSON() (string, error) {
	data, err := json.MarshalIndent(r.Records(), "", "  ")
	if err != nil {
		return "", err
	}
	return string(data), nil
}
