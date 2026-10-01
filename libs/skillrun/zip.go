package skillrun

// skill 包 zip 的读侧校验与解压（schema/规则与 aic skillhub zip.go 同一契约，
// 分模块不互引——规则改动必须同步 docs/skill.md §3/§9.2）。
//
// 读侧防御：单文件 64MB / 解压总量 256MB；条目路径 zip-slip 全拒。

import (
	"archive/zip"
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// zip 体积读侧防御上限（var 供测试降值）。
var (
	maxZipFileSize  int64 = 1 << 26
	maxZipTotalSize int64 = 1 << 28
)

// zipEntry 解析后的 zip 文件（相对路径 + 内容）。
type zipEntry struct {
	name string
	data []byte
}

// readZipEntries 读取全部普通文件条目（超限/不安全条目显式报错）。
func readZipEntries(zipData []byte) ([]zipEntry, error) {
	zr, err := zip.NewReader(bytes.NewReader(zipData), int64(len(zipData)))
	if err != nil {
		return nil, err
	}
	files := make([]zipEntry, 0, len(zr.File))
	var total int64
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		name, ok := sanitizeZipEntry(f.Name)
		if !ok {
			return nil, fmt.Errorf("unsafe zip entry %q", f.Name)
		}
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		data, err := io.ReadAll(io.LimitReader(rc, maxZipFileSize+1))
		_ = rc.Close()
		if err != nil {
			return nil, err
		}
		if int64(len(data)) > maxZipFileSize {
			return nil, fmt.Errorf("zip entry %q exceeds single-file limit", f.Name)
		}
		total += int64(len(data))
		if total > maxZipTotalSize {
			return nil, fmt.Errorf("zip total size exceeds limit")
		}
		files = append(files, zipEntry{name: name, data: data})
	}
	return files, nil
}

// sanitizeZipEntry 清洗 zip 内路径（\ → /，拒绝对路径/.. 段/盘符）。
func sanitizeZipEntry(raw string) (string, bool) {
	raw = strings.TrimSpace(strings.ReplaceAll(raw, `\`, "/"))
	if raw == "" || strings.HasPrefix(raw, "/") {
		return "", false
	}
	clean := filepath.ToSlash(filepath.Clean(raw))
	if clean == "." || clean == "" {
		return "", false
	}
	for _, seg := range strings.Split(clean, "/") {
		if seg == ".." || strings.Contains(seg, ":") {
			return "", false
		}
	}
	return clean, true
}

// extractZipEntries 解压到 destReal（sanitize 已拦 zip-slip）。
func extractZipEntries(files []zipEntry, destReal string) error {
	if err := os.MkdirAll(destReal, 0o755); err != nil {
		return err
	}
	for _, f := range files {
		target := filepath.Join(destReal, filepath.FromSlash(f.name))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(target, f.data, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// validateZipCLI 包 CLI 校验（与注册中心 validateZipCLI 同口径）：含 cli/
// 条目 → cli/manifest.json 必须存在且有效；cli/artifacts.lock.json 存在 →
// 必须有效。返回解析出的 manifest（无 cli/ = nil——非 CLI 包也允许安装：
// download 是通用能力，带 scripts/ 等资源的技能同用）。
func validateZipCLI(files []zipEntry) (*Manifest, *ArtifactsLock, error) {
	const cliDir = "cli/"
	hasCLI := false
	for _, f := range files {
		if strings.HasPrefix(f.name, cliDir) {
			hasCLI = true
			break
		}
	}
	if !hasCLI {
		return nil, nil, nil
	}
	var m *Manifest
	var lock *ArtifactsLock
	hasManifest := false
	for _, f := range files {
		switch f.name {
		case cliDir + "manifest.json":
			hasManifest = true
			var err error
			m, err = ParseManifestBytes(f.data)
			if err != nil {
				return nil, nil, fmt.Errorf("cli/manifest.json: %w", err)
			}
		case cliDir + "artifacts.lock.json":
			var err error
			lock, err = ParseArtifactsLock(f.data)
			if err != nil {
				return nil, nil, fmt.Errorf("cli/artifacts.lock.json: %w", err)
			}
		}
	}
	if !hasManifest {
		return nil, nil, fmt.Errorf("cli/ present but cli/manifest.json missing")
	}
	return m, lock, nil
}

// findZipEntry 按名取条目（无 = nil）。
func findZipEntry(files []zipEntry, name string) *zipEntry {
	for i := range files {
		if files[i].name == name {
			return &files[i]
		}
	}
	return nil
}
