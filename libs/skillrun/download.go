package skillrun

// skill download 的原子安装序列与安装记录（v6 P2，docs/skill.md §9.2）：
// fetch 暂存 → 校验 manifest → 包名冲突全检 → 停旧 provider → 目录切换 →
// 写 .install.json（**最后写 = 提交标记**）→ 注册。启动扫描只认带有效
// .install.json 的目录，半包不注册。来源身份 = kind(local|public|builtin)+id：
// 同源同名 = 更新（停 provider、替换目录、重新注册）；异源同名 = 显式报错。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
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

// Download 经 NATS fetch 下载并原子安装。ref = 注册表 uuid 或云端本地目录名。
func (r *Registry) Download(ctx context.Context, ref, version string) (*Package, error) {
	if r.deps.Fetch == nil {
		return nil, fmt.Errorf("skillrun: 此端未接包获取通道")
	}
	zipData, meta, err := r.deps.Fetch(ctx, ref, version)
	if err != nil {
		return nil, err
	}
	return r.installZip(ctx, zipData, meta)
}

// installZip 原子安装序列（Download 与后续 builtin 首跑安装共用）。
func (r *Registry) installZip(ctx context.Context, zipData []byte, meta *FetchMeta) (*Package, error) {
	// 暂存校验：zip 完整性与 CLI 契约（含 cli/ 必有有效 manifest）。
	files, err := readZipEntries(zipData)
	if err != nil {
		return nil, fmt.Errorf("skill download: bad zip: %w", err)
	}
	manifest, lock, err := validateZipCLI(files)
	if err != nil {
		return nil, fmt.Errorf("skill download: %w", err)
	}
	if !namePattern.MatchString(meta.Name) {
		return nil, fmt.Errorf("skill download: invalid package name %q", meta.Name)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	// 来源裁决：同源同名 = 更新；异源同名 = 显式报错不覆盖。
	var rec InstallRecord
	if old, exists := r.pkgs[meta.Name]; exists {
		oldRec := old.record
		if oldRec.Kind != meta.Kind || oldRec.ID != meta.ID {
			return nil, fmt.Errorf("skill download: %q 已安装且来源不同（%s:%s vs %s:%s）——先卸载再安装", meta.Name, oldRec.Kind, oldRec.ID, meta.Kind, meta.ID)
		}
		rec.Disabled = oldRec.Disabled // 更新保留禁用态
	}
	// 包名冲突全检（新装）：vs 内建/保留名/已装包。
	reg, err := r.deps.Registry()
	if err != nil {
		return nil, fmt.Errorf("skillrun: engine registry: %w", err)
	}
	if _, exists := r.pkgs[meta.Name]; !exists {
		if _, taken := reg.Lookup(meta.Name); taken {
			return nil, fmt.Errorf("skill download: command %q already registered（包名冲突，显式拒绝）", meta.Name)
		}
	}
	// 停旧 provider + 解注册（更新路径；Unregister 幂等）。
	r.killServicesLocked(meta.Name)
	reg.Unregister(meta.Name)

	// 目录切换：解压 .next → 删旧 → 上位（失败尽力回滚）。
	final := filepath.Join(r.deps.SkillsDir, meta.Name)
	next := final + ".next"
	_ = os.RemoveAll(next)
	if err := extractZipEntries(files, next); err != nil {
		return nil, fmt.Errorf("skill download: extract: %w", err)
	}
	if _, err := os.Stat(final); err == nil {
		backup := final + ".old"
		_ = os.RemoveAll(backup)
		if err := os.Rename(final, backup); err != nil {
			_ = os.RemoveAll(next)
			return nil, fmt.Errorf("skill download: swap: %w", err)
		}
		defer os.RemoveAll(backup)
	}
	if err := os.Rename(next, final); err != nil {
		return nil, fmt.Errorf("skill download: promote: %w", err)
	}
	// entry 执行位 + artifacts.lock 设备侧下载（失败 = 安装失败；无
	// .install.json = 半包，启动扫描不注册——显式报错由用户重试/卸载）。
	if manifest != nil {
		for _, p := range manifest.Providers {
			if err := os.Chmod(filepath.Join(final, filepath.FromSlash(p.Entry)), 0o755); err != nil {
				return nil, fmt.Errorf("skill download: provider %q entry exec bit: %w", p.ID, err)
			}
		}
	}
	if lock != nil {
		if err := r.fetchArtifacts(ctx, lock, final); err != nil {
			return nil, fmt.Errorf("skill download: %w", err)
		}
	}
	// 写 .install.json = 提交标记（最后写）。
	rec.Name = meta.Name
	rec.Kind = meta.Kind
	rec.ID = meta.ID
	rec.Version = meta.Version
	rec.InstalledAt = time.Now().UTC()
	if err := writeRecord(final, rec); err != nil {
		return nil, fmt.Errorf("skill download: write install record: %w", err)
	}
	pkg := &Package{Name: meta.Name, Dir: final, Manifest: manifest, record: rec}
	pkg.disabled = rec.Disabled
	// 注册根命令（仅 CLI 包；非 CLI 资源包无根命令）。
	if manifest != nil {
		if err := reg.RegisterGuarded(r.rootCommand(pkg)); err != nil {
			return nil, fmt.Errorf("skill download: register root command: %w", err)
		}
	}
	r.pkgs[meta.Name] = pkg
	r.logf("skillrun: downloaded %s (%s:%s@%s) -> %s", meta.Name, meta.Kind, meta.ID, meta.Version, final)
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
	if err := json.Unmarshal(data, &rec); err != nil || rec.Name == "" || rec.Kind == "" || rec.ID == "" {
		return nil
	}
	return &rec
}

// Rescan 启动扫描重注册：只认带有效 .install.json 的目录（半包不注册）；
// CLI 包（有效 cli/manifest.json）注册根命令，禁用态随记录恢复。
// 引擎尚未构建的注册表冲突逐包日志跳过（重启自恢复不阻断启动）。
func (r *Registry) Rescan() {
	entries, err := os.ReadDir(r.deps.SkillsDir)
	if err != nil {
		return
	}
	reg, err := r.deps.Registry()
	if err != nil {
		r.logf("skillrun: rescan: engine registry unavailable: %v", err)
		return
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(r.deps.SkillsDir, e.Name())
		rec := readRecord(dir)
		if rec == nil {
			continue // 半包不注册
		}
		var manifest *Manifest
		mpath := filepath.Join(dir, "cli", "manifest.json")
		if _, err := os.Stat(mpath); err == nil {
			m, err := ParseManifest(mpath)
			if err != nil {
				r.logf("skillrun: rescan: %s manifest invalid, skipped: %v", rec.Name, err)
				continue
			}
			manifest = m
		}
		pkg := &Package{Name: rec.Name, Dir: dir, Manifest: manifest, record: *rec}
		pkg.disabled = rec.Disabled
		r.mu.Lock()
		r.pkgs[rec.Name] = pkg
		r.mu.Unlock()
		if manifest != nil {
			if err := reg.RegisterGuarded(r.rootCommand(pkg)); err != nil {
				r.logf("skillrun: rescan: register %s: %v", rec.Name, err)
			}
		}
		r.logf("skillrun: rescan: %s (%s:%s@%s) registered", rec.Name, rec.Kind, rec.ID, rec.Version)
	}
}

// Records 全部安装记录（skill list --json 数据源；按包名排序）。
func (r *Registry) Records() []InstallRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]InstallRecord, 0, len(r.pkgs))
	for _, pkg := range r.pkgs {
		out = append(out, pkg.record)
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
