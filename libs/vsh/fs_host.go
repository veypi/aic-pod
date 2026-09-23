package vsh

import (
	"fmt"
	"strings"

	"github.com/veypi/vbox"
	gbfs "github.com/veypi/vsh/fs"
	"github.com/veypi/vigo/contrib/ufs"
)

// HostFSConfig host 会话文件系统参数（design §4.2）。
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
// 本就是 scratch，无 cloud 污染红线；stub 钉会话级真实目录，见 PinStubs）。
func NewHostFS(cfg HostFSConfig) (gbfs.FileSystem, error) {
	if cfg.Backing == nil {
		return nil, fmt.Errorf("vsh glue: host backing required")
	}
	return NewUFSAdapter(UFSAdapterConfig{
		Backing: cfg.Backing,
		Rules:   cfg.Rules,
		// 无 jail：host 的边界由规则表表达（读默认开放、写白名单制）。
		// 无内存层：stub 落 {session_root}/{sid}/bin 真实目录。
	})
}

// StubDirFor 返回会话 stub 目录（design §4.2：PATH 钉 {session_root}/{sid}/bin，
// fsauth 基础白名单内、天然可写，随会话清理）。
func StubDirFor(sessionRoot, sid string) string {
	return strings.TrimSuffix(sessionRoot, "/") + "/" + sid + "/bin"
}

// PinStubs 把 stub 写入会话级真实目录（每次 exec 前调用，幂等覆盖）。
// stub 虽在写域内可写，D14 registry 优先：同名真实文件无法 shadow 平台命令。
func PinStubs(backing ufs.FS, stubDir string, stubs map[string][]byte) error {
	if len(stubs) == 0 {
		return nil
	}
	if err := backing.MkdirAll(stubDir, 0o755); err != nil {
		return fmt.Errorf("vsh glue: pin stubs mkdir: %w", err)
	}
	for name, data := range stubs {
		if strings.Contains(name, "/") || name == "" || name == "." || name == ".." {
			return fmt.Errorf("vsh glue: invalid stub name %q", name)
		}
		if err := backing.WriteFile(stubDir+"/"+name, data, 0o755); err != nil {
			return fmt.Errorf("vsh glue: pin stub %s: %w", name, err)
		}
	}
	return nil
}
