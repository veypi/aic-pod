// Copyright (C) 2025 veypi <i@veypi.com>
// Distributed under terms of the MIT license.

// Package skillrun 是 skill 包在 pod 端的安装/注册/运行权威（aic/docs/skill.md
// §9.2，v6）：包命令注册表生命周期收归本包一处——manifest 解析校验、包名冲突
// 检查（vs 内建/保留名/已装包）、根命令注册（vsh RegisterGuarded）、禁用=保留
// 注册显式失败、卸载=解注册（+bg kill，service 于 P0b 接线）、整包替换。
//
// 运行形态：
//   - process 类：每调用一次独立 vbox 沙箱进程（与 native 命令同一沙箱派生——
//     策略快照源复用 host.nativePolicy），stdin/stdout/stderr 直通引擎管道，
//     ctx 取消即杀进程组（受管取消）。
//   - service 类：首调用懒启动驻留 + bg 登记 + skillproc 拨号（P0b）。
//
// 根命令 = 包名（隐式，manifest 无 commands[]）；argv/stdin 全量透传给包的
// 默认 provider（providers[0]），子命令与 --help 由包 CLI 自行实现。
package skillrun

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Manifest 是 cli/manifest.json 的结构（v6 schema：只声明 providers[] 与可选
// streams[]；根命令 = 包名隐式，不登记 commands[]）。
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

// Provider 形态。
const (
	KindProcess = "process"
	KindService = "service"
)

// ParseManifest 读取并校验 cli/manifest.json。
func ParseManifest(path string) (*Manifest, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseManifestBytes(b)
}

// ParseManifestBytes 解析并校验 manifest JSON 字节。
func ParseManifestBytes(b []byte) (*Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
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
		if err := validateEntry(p.Entry); err != nil {
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

// validateEntry：相对路径、clean 后不逃逸包目录。
func validateEntry(entry string) error {
	if strings.TrimSpace(entry) == "" {
		return fmt.Errorf("is required")
	}
	if filepath.IsAbs(entry) || strings.HasPrefix(entry, "~") {
		return fmt.Errorf("must be relative to package dir: %q", entry)
	}
	clean := filepath.ToSlash(filepath.Clean(entry))
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return fmt.Errorf("escapes package dir: %q", entry)
	}
	return nil
}

// Default 返回包的默认 provider（providers[0]——根命令的运行目标）。
func (m *Manifest) Default() Provider { return m.Providers[0] }
