package skillrun

// builtin skill 包首跑预装（v6 P5）：随安装介质分发的 zip（desktop 打包把
// browser.zip 带进 resources，spawn 后端时注入 AIC_BUILTIN_SKILLS，路径列表
// 以平台分隔符分隔），pod 启动扫描后逐项安装——安装语义与 Download 完全同一
// 条 installZip 原子序列（来源身份 kind=builtin, id=包名；包名/版本取自 zip
// 内 SKILL.md frontmatter）。
//
// 幂等：已装记录为同源 builtin 且版本一致 → 跳过；同名异源 → installZip 显式
// 报错（与 Download 同语义）。任何单项失败只记日志不阻断启动——浏览器能力
// 降级为未安装，其余功能照常。

import (
	"context"
	"fmt"
	"os"
	"strings"
)

// KindBuiltin builtin 来源标识（安装记录 kind；id = 包名）。
const KindBuiltin = "builtin"

// Preinstall 逐项预装 builtin zip（启动序列：Rescan 之后、connect 之前调用；
// 单项失败记日志继续）。
func (r *Registry) Preinstall(ctx context.Context, zipPaths []string) {
	for _, p := range zipPaths {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if err := r.installBuiltin(ctx, p); err != nil {
			r.logf("skillrun: builtin preinstall %s: %v", p, err)
		}
	}
}

// installBuiltin 预装单个 zip：读 bytes → 解析包身份（SKILL.md frontmatter
// name/version）→ 同源同版本跳过 → installZip。
func (r *Registry) installBuiltin(ctx context.Context, zipPath string) error {
	if !strings.HasSuffix(strings.ToLower(zipPath), ".zip") {
		return fmt.Errorf("not a zip: %s", zipPath)
	}
	data, err := os.ReadFile(zipPath)
	if err != nil {
		return err
	}
	files, err := readZipEntries(data)
	if err != nil {
		return fmt.Errorf("bad zip: %w", err)
	}
	var skillDoc []byte
	if e := findZipEntry(files, "SKILL.md"); e != nil {
		skillDoc = e.data
	}
	name, version := skillFrontmatter(skillDoc)
	if name == "" {
		return fmt.Errorf("SKILL.md frontmatter name missing")
	}
	meta := &FetchMeta{Name: name, Kind: KindBuiltin, ID: name, Version: version}
	// 幂等：同源 builtin 同名同版本 → 跳过（不重装）。
	if old := r.Get(name); old != nil {
		rec := old.Record()
		if rec.Kind == meta.Kind && rec.ID == meta.ID && rec.Version == meta.Version {
			r.logf("skillrun: builtin %s@%s already installed, skipped", name, version)
			return nil
		}
	}
	_, err = r.installZip(ctx, data, meta)
	return err
}

// skillFrontmatter 读 SKILL.md frontmatter 顶层 name/version（`---` 围栏内
// 无缩进 `key: value`；不引 yaml 依赖，只取两个标量键）。
func skillFrontmatter(doc []byte) (name, version string) {
	lines := strings.Split(string(doc), "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return "", ""
	}
	for _, line := range lines[1:] {
		if strings.TrimSpace(line) == "---" {
			break
		}
		if line == "" || line[0] == ' ' || line[0] == '\t' || line[0] == '#' {
			continue // 只取顶层标量键
		}
		key, value, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		value = strings.TrimSpace(value)
		switch strings.TrimSpace(key) {
		case "name":
			name = value
		case "version":
			version = value
		}
	}
	return name, version
}
