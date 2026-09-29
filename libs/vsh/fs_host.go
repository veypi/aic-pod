package vsh

import (
	"context"
	"fmt"
	stdfs "io/fs"

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
// 本就是 scratch，无 cloud 污染红线；stub 钉进程级 .vsh-host/bin 真实目录，
// 见 libs/host/engine_vsh.go 文件头偏差 1）。
func NewHostFS(cfg HostFSConfig) (gbfs.FileSystem, error) {
	if cfg.Backing == nil {
		return nil, fmt.Errorf("vsh glue: host backing required")
	}
	fsys, err := NewUFSAdapter(UFSAdapterConfig{
		Backing: cfg.Backing,
		Rules:   cfg.Rules,
		// 无 jail：host 的边界由规则表表达（读默认开放、写白名单制）。
		// 无内存层：stub 落 {session_root}/{sid}/bin 真实目录。
	})
	if err != nil {
		return nil, err
	}
	return hostLayoutFS{fsys}, nil
}

// hostLayoutFS host 布局特化：布局初始化（每次 NewSession）会对 /tmp 做
// MkdirAll + Chmod(sticky|0777)——posix 真实 OS 的 /tmp 本已存在且为
// sticky|1777，非 root chmod 必 EPERM；windows 的 /tmp 是 OSVFS 虚拟别名
// （映射 os.TempDir()，必已存在）。两个调用对精确路径 /tmp noop（只拦 /tmp
// 本身；/tmp 下子路径照常委派 backing——win 上经虚拟别名落临时目录）。
type hostLayoutFS struct{ gbfs.FileSystem }

func (h hostLayoutFS) MkdirAll(ctx context.Context, name string, perm stdfs.FileMode) error {
	if gbfs.Clean(name) == "/tmp" {
		return nil
	}
	return h.FileSystem.MkdirAll(ctx, name, perm)
}

func (h hostLayoutFS) Chmod(ctx context.Context, name string, mode stdfs.FileMode) error {
	if gbfs.Clean(name) == "/tmp" {
		return nil
	}
	return h.FileSystem.Chmod(ctx, name, mode)
}
