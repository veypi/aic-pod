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

	aicskills "github.com/veypi/aic-skills"
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

// PreinstallEmbedded 二进制内嵌包预装（aicskills；v6.1 内建机制——pod
// 二进制自带内建 skill，设备零下载）。与 zip 预装同一 installZip 原子序列、
// 同一幂等语义；单项失败记日志继续。
func (r *Registry) PreinstallEmbedded(ctx context.Context) {
	for _, name := range aicskills.List() {
		data, err := aicskills.Zip(name)
		if err != nil {
			r.logf("skillrun: embedded builtin %s: %v", name, err)
			continue
		}
		if err := r.installBuiltinZip(ctx, data, "embedded:"+name); err != nil {
			r.logf("skillrun: embedded builtin %s: %v", name, err)
		}
	}
}

// installBuiltin 预装单个 zip：读 bytes → installBuiltinZip。
func (r *Registry) installBuiltin(ctx context.Context, zipPath string) error {
	if !strings.HasSuffix(strings.ToLower(zipPath), ".zip") {
		return fmt.Errorf("not a zip: %s", zipPath)
	}
	data, err := os.ReadFile(zipPath)
	if err != nil {
		return err
	}
	return r.installBuiltinZip(ctx, data, zipPath)
}

// installBuiltinZip builtin 预装主体：解析包身份（SKILL.md frontmatter
// name/version）→ 同源同版本跳过 → installZip。origin 仅用于日志。
func (r *Registry) installBuiltinZip(ctx context.Context, data []byte, origin string) error {
	files, err := readZipEntries(data)
	if err != nil {
		return fmt.Errorf("bad zip %s: %w", origin, err)
	}
	var skillDoc []byte
	if e := findZipEntry(files, "SKILL.md"); e != nil {
		skillDoc = e.data
	}
	name, version := aicskills.Frontmatter(skillDoc)
	if name == "" {
		return fmt.Errorf("%s: SKILL.md frontmatter name missing", origin)
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
