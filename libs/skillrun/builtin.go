package skillrun

// 内建包只有二进制嵌入一个来源，安装复用 installZip 的原子更新与来源检查。

import (
	"context"
	"fmt"

	aicskills "github.com/veypi/aic-skills"
)

// KindBuiltin builtin 来源标识（安装记录 kind；id = 包名）。
const KindBuiltin = "builtin"

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
	_, err = r.InstallZip(ctx, data, meta)
	return err
}
