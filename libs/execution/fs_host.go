package execution

import (
	"fmt"

	"github.com/veypi/vbox"
	"github.com/veypi/vigo/contrib/ufs"
	gbfs "github.com/veypi/vsh/fs"
)

// HostFSConfig host 会话文件系统参数。
type HostFSConfig struct {
	// Backing OS 文件系统适配（libs/host OSVFS 或其 view）。host 侧真实路径
	// 含 symlink，规则表门 canonicalize-then-check 在 ufsAdapter 内完成
	//（vbox.Match 入口 canonical 化，host 与 cloud 同一 matcher）。
	Backing ufs.FS
	// Rules vbox 规则表快照源（host 行序：temp → cfg/permanent → builtin deny
	// → 便利根）。用户态执行与 OS 沙箱同源（一套策略源，两种执行机制）。
	Rules func() vbox.FSRuleSet
}

// NewHostFS host：OS backing + vbox 规则表门，不引内存覆盖层（host 会话目录
// 本就是 scratch）。FS factory 不生成命令文件。
// （pod 自身机械读写不过门，见 libs/host/engine_vsh.go 文件头决策 1）。
func NewHostFS(cfg HostFSConfig) (gbfs.FileSystem, error) {
	if cfg.Backing == nil {
		return nil, fmt.Errorf("vsh glue: host backing required")
	}
	return NewUFSAdapter(UFSAdapterConfig{
		Backing: cfg.Backing,
		Rules:   cfg.Rules,
		NormalizePath: func(p string, noFollow bool) string {
			if noFollow {
				return vbox.CanonicalNoFollow(p)
			}
			return vbox.Canonical(p)
		},
		// 无 jail：host 的边界由规则表表达（读默认开放、写白名单制）。
		// 无内存层：会话 FS 直读直写宿主盘（规则门拦截越界写）。
	})
}
