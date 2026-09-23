package fsx

// env.go 是 fsx（fs 工具薄层，自 vcore 移植，cloud/host 共用）的执行
// 环境：全部文件操作经 FS（cloud = QuotaFS 包装的 ufs.FS；host = hostfs
// View）；路径经 Resolve 归一（+jail 由调用方实现）；权限经 Gate（vbox
// 规则表门，cloud 接 CloudFSRules，host 接 fsauth vbox 快照）。read/ls/rg
// 开放（读默认放行由规则表保证）；write/edit 出区由 Gate 拒绝并引导 grant fs。

import (
	"path"
	"strings"

	"github.com/veypi/vigo/contrib/ufs"
)

// Env 是 fsx 薄层环境（自 vcore.Env 收敛：剥除 VirtualRoot/Roots/ProtectRoots/
// Fetcher/Tasks/Vars——cloud-only，那些职责由引擎/glue 与调用方承接）。
type Env struct {
	// FS 是文件操作目标（ufs.FS 可写接口；调用方包 QuotaFS）。
	FS ufs.FS
	// Workdir 是缺省基准目录（ls/rg 省略 path 时的缺省值）。
	Workdir string
	// ImageData 为 true 时图片 read 经 image_data（data URI）返回；
	// false（cloud）时返回 image_path（§2.2 图片标准）。
	ImageData bool
	// Gate 是权限门：op 形如 "fs write"；write=true 查写权限。
	// 由调用方接 vbox 规则表（出区返回可读错误引导 grant fs）；nil = 全放行。
	Gate func(op, abs string, write bool) error
	// VirtualRoot 为 true 时 FS 根 "/" 是虚拟挂载列表（host windows 盘符根，
	// ls/rg 根目录特判）；cloud 恒 false。
	VirtualRoot bool
	// Resolve 把调用路径归一为 FS 内绝对路径（jail 由调用方实现：
	// 越界返回错误）；nil = 词法绝对化（相对 → Workdir 下）。
	ResolveFunc func(p string) (string, error)
}

// Resolve 归一路径（相对 → Workdir 下；词法 clean；委托 ResolveFunc）。
func (e *Env) Resolve(p string) (string, error) {
	if e.ResolveFunc != nil {
		return e.ResolveFunc(p)
	}
	if !strings.HasPrefix(p, "/") {
		base := e.Workdir
		if base == "" {
			base = "/"
		}
		p = path.Join(base, p)
	}
	return path.Clean(p), nil
}

// CheckPath 是移植兼容层（vcore 的 Roots 收容）：fsx 的收容由 Gate 统一
// 承担——读向检查（越 jail 在 Resolve 已拦，规则表读默认开放）。
func (e *Env) CheckPath(action, abs string) error {
	if e.Gate == nil {
		return nil
	}
	return e.Gate(action, abs, false)
}

// CheckPolicy 规则表门（写判定）：op 形如 "fs write"。
func (e *Env) CheckPolicy(op, abs string, write bool) error {
	if e.Gate == nil {
		return nil
	}
	return e.Gate(op, abs, write)
}

// fsErr 定义在 result.go（移植沿用）。
