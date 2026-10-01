package skillrun

// artifacts.lock.json 大二进制声明（schema 与 aic skillhub 同一契约，
// docs/skill.md §9.2）：来源/摘要/落盘位置；摘要只用于安装时校验下载完整性。
// 设备侧网络下载验 sha256，不经平台中转。

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

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

// ParseArtifactsLock 解析并全量校验。
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
		if err := validateEntry(a.Path); err != nil {
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
