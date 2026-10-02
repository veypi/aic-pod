// Package skillpkg — cli/manifest.json 与 artifacts.lock.json 的共享契约
// 契约定位：aic docs/skill.md §9.2（v6）。aic skillhub 发布/内建校验与 aic-pod
// skillrun 安装/运行同一真相源——schema 与校验规则改动只改本包。
//
// manifest：providers[]（id、kind=process|service、entry、args）+ 可选
// streams[]（name、provider）；根命令 = 包名隐式，不登记 commands[]。
// artifacts.lock：大型二进制声明（来源/摘要/落盘位置），摘要只用于安装时
// 校验下载完整性。
package skillpkg

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

// ManifestFile cli 能力清单（提供 cli 时必有，有效才算 CLI 能力）。
const ManifestFile = "manifest.json"

// ArtifactsLockFile 大二进制声明（可选，设备侧下载 + sha256 校验）。
const ArtifactsLockFile = "artifacts.lock.json"

// Provider 形态。
const (
	KindProcess = "process"
	KindService = "service"
)

// Manifest 是 cli/manifest.json 的结构。
type Manifest struct {
	Providers []Provider   `json:"providers"`
	Streams   []StreamDecl `json:"streams,omitempty"`
}

// Provider 是一个能力提供者。entry 相对包安装目录，不可逃逸。
type Provider struct {
	ID    string   `json:"id"`             // 包内唯一
	Kind  string   `json:"kind"`           // process | service
	Entry string   `json:"entry"`          // 可执行文件（相对包目录）
	Args  []string `json:"args,omitempty"` // 固定前导参数（argv 透传之前）
}

// StreamDecl 声明一条二进制流端点，由指定 provider 服务。
type StreamDecl struct {
	Name     string `json:"name"`
	Provider string `json:"provider"`
}

// ParseManifest 解析并全量校验 manifest JSON 字节。
func ParseManifest(data []byte) (*Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("manifest: bad json: %w", err)
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return &m, nil
}

// Validate 全量校验：provider 非空、id 唯一非空、kind 合法、entry 相对不逃逸、
// stream 引用存在的 provider。
func (m *Manifest) Validate() error {
	if len(m.Providers) == 0 {
		return fmt.Errorf("manifest: providers is required")
	}
	ids := map[string]bool{}
	for i, p := range m.Providers {
		if strings.TrimSpace(p.ID) == "" {
			return fmt.Errorf("manifest: providers[%d].id is required", i)
		}
		if ids[p.ID] {
			return fmt.Errorf("manifest: duplicate provider id %q", p.ID)
		}
		ids[p.ID] = true
		switch p.Kind {
		case KindProcess, KindService:
		default:
			return fmt.Errorf("manifest: providers[%d].kind %q must be process|service", i, p.Kind)
		}
		if err := ValidateRelPath(p.Entry); err != nil {
			return fmt.Errorf("manifest: providers[%d].entry: %w", i, err)
		}
	}
	for i, s := range m.Streams {
		if strings.TrimSpace(s.Name) == "" {
			return fmt.Errorf("manifest: streams[%d].name is required", i)
		}
		if !ids[s.Provider] {
			return fmt.Errorf("manifest: streams[%d].provider %q not found", i, s.Provider)
		}
	}
	return nil
}

// Default 返回包的默认 provider（providers[0]——根命令的运行目标）。
func (m *Manifest) Default() Provider { return m.Providers[0] }

// ArtifactsLock artifacts.lock.json 结构。
type ArtifactsLock struct {
	Artifacts []Artifact `json:"artifacts"`
}

// Artifact 一条大二进制声明。
type Artifact struct {
	Path   string `json:"path"`   // 落盘位置（相对包目录，不可逃逸）
	URL    string `json:"url"`    // 设备侧下载来源（https）
	SHA256 string `json:"sha256"` // 摘要（小写 64 hex）
}

var sha256Re = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ParseArtifactsLock 解析并全量校验：artifacts 非空、path 相对不逃逸、
// url 仅 https、sha256 形态合法、path 不重复。
func ParseArtifactsLock(data []byte) (*ArtifactsLock, error) {
	var l ArtifactsLock
	if err := json.Unmarshal(data, &l); err != nil {
		return nil, fmt.Errorf("artifacts.lock: bad json: %w", err)
	}
	if len(l.Artifacts) == 0 {
		return nil, fmt.Errorf("artifacts.lock: artifacts is required")
	}
	seen := map[string]bool{}
	for i, a := range l.Artifacts {
		if err := ValidateRelPath(a.Path); err != nil {
			return nil, fmt.Errorf("artifacts.lock: artifacts[%d].path: %w", i, err)
		}
		if seen[a.Path] {
			return nil, fmt.Errorf("artifacts.lock: duplicate path %q", a.Path)
		}
		seen[a.Path] = true
		if !strings.HasPrefix(a.URL, "https://") {
			return nil, fmt.Errorf("artifacts.lock: artifacts[%d].url must be https", i)
		}
		if !sha256Re.MatchString(a.SHA256) {
			return nil, fmt.Errorf("artifacts.lock: artifacts[%d].sha256 must be 64 lowercase hex", i)
		}
	}
	return &l, nil
}

// ValidateRelPath：相对路径、clean 后不逃逸包目录。
func ValidateRelPath(p string) error {
	if strings.TrimSpace(p) == "" {
		return fmt.Errorf("is required")
	}
	if filepath.IsAbs(p) || strings.HasPrefix(p, "~") {
		return fmt.Errorf("must be relative to package dir: %q", p)
	}
	clean := filepath.ToSlash(filepath.Clean(p))
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return fmt.Errorf("escapes package dir: %q", p)
	}
	return nil
}
