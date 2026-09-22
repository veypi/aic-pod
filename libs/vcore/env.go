package vcore

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/veypi/aic-pod/libs/proto"
	"github.com/veypi/vigo/contrib/ufs"
)

// Env 是指令执行环境：VFS + 路径解析上下文 + 策略钩子。
// 权限判定、网络策略等由引入方注入，vcore 不内建。
type Env struct {
	// VFS 是全部文件指令作用的抽象文件系统（vigo ufs.FS 可写接口）。
	// server 端适配 UFS（chroot 到会话空间 + 授权等级包装），pod 端适配 OS
	// 本地路径，测试使用 MemVFS。路径为斜杠分隔的绝对路径。nil 表示文件
	// 服务未开启——需要文件能力的指令必须先经引入方的 fs 门控判定。
	VFS     ufs.FS
	Workdir string            // 当次调用显式携带的基准目录（缺省值由调用方先行填充，§2.1.1）
	Vars    map[string]string // 根变量映射（预留；三端当前均无变量）；nil = 物理 host
	// Roots 是可访问根列表（§2.1.1 执行层收容）：展开后的路径必须位于某个 root 内
	// （含 root 自身），否则 DeniedError（不可审批绕过）。
	// cloud = ["/"]（VFS chroot 到会话空间）；nil = 物理 host 不限制（用户机器自身边界）。
	Roots []string
	// ProtectRoots 是 rm/mv 的根目录硬保护列表（§5.4：cloud 会话空间根 / 物理 host 文件系统根）。
	// 命中返回 DeniedError，不可审批绕过。
	ProtectRoots []string
	// VirtualRoot 为 true 时 VFS 根 "/" 是虚拟挂载列表（windows 盘符根）：
	// ls 根层挂载条目不递归（仅显示，进入需显式 ls 该盘符，防 tree 默认深度
	// 递归进全部盘符）；rg 拒绝 path="/"（遍历全部盘符无界，须指定盘符路径）。
	VirtualRoot bool
	// Fetcher 是 curl 的 HTTP 获取器（SSRF 等策略由引入方注入）；nil = curl 不可用。
	Fetcher Fetcher
	// Tasks 是托管任务运行器（curl 无 -o 等长输出/长耗时指令）：输出落盘日志、
	// 请求超时自动后台化、bg_* 统一管理；nil = 任务形态不可用（curl 必须 -o）。
	Tasks TaskRunner
	// TaskID 是当次调用的任务 ID（工具调用 msg_id），由调用方在构造 Env 时填充。
	TaskID string
	// ImageData 为 true（host/page）时图片 read 经 image_data（data URI）返回；
	// false（cloud）时返回 image_path（§2.2 图片标准）。
	ImageData bool
	// Policy 是统一文件权限模型（aic todo v0.14.5 §2；fsauth.Policy 的会话视图）：
	// 文件类指令按 canonical 路径动态升级 required——deny → DeniedError（0 级，
	// 读写双拒，不可审批绕过）；写白名单外（写 0 级）同样 DeniedError——本地
	// 规则，grant/审批不可绕过；可写白名单/open → 1/2，按 granted 数字比较放行。
	// nil = 无路径策略（cloud 信任域 GatedFS 独立分级 / page）。
	Policy PathPolicy
	// Granted 是当次调用的授予等级（host = req.GrantedLevel；审批通过 = 9）。
	// Policy 升级判定用：granted >= need 直接放行。
	Granted int
}

// PathPolicy 是 Env 的文件路径策略接口（v0.14.5 §2 注入式：实现由 fsauth 提供，
// vcore 只依赖签名——fsauth 侧 canonical 判定，注入侧保证 fs 与 exec 同实例）。
type PathPolicy interface {
	// Decide 返回路径的 (read, write) 所需等级：deny → 0/0；可写白名单/open
	// → 1/2；其余 → 1/0（读默认开放，写仅白名单）；deny 始终优先。
	// 入参为 Resolve/CheckPath 之后的绝对路径；实现内部做 canonical 展开。
	Decide(path string) (read, write int)
}

// NoFollowPolicy 是可选的策略扩展：rm/mv 等不跟随末段的操作用 unlink 语义判定
// （fsauth.View 实现；未实现的策略回退 Decide——测试桩与旧实现语义不变）。
type NoFollowPolicy interface {
	// DecideNoFollow 同 Decide，但末段符号链接不展开（删/挪的是链接本身）。
	DecideNoFollow(path string) (read, write int)
}

// CheckPolicy 文件类指令的路径策略门（Policy 非 nil 时）：write=false 查 read 级。
// deny 命中（读 0 级）→ DeniedError（本地 deny 读写双拒，审批/grant 不可绕过）；
// granted >= need → 放行；否则 DeniedError 并提示可申请 grant（白名单外写可经
// grant fs 扩写白名单后重发；审批通过 granted=9 重发放行——与 GatedFS/host
// checkGranted 同语义）。
// 导出供 vcore 子包（browser 文件交换）使用——文件字节经 VFS 落盘的一切通道都必须过此门。
func (e *Env) CheckPolicy(op, abs string, write bool) error {
	if e.Policy == nil {
		return nil
	}
	rd, wr := e.Policy.Decide(abs)
	return e.checkGrades(op, abs, rd, wr, write)
}

// CheckPolicyUnlink 同 CheckPolicy，但按 unlink/rename 语义判定（末段不跟随
// 符号链接；实现 NoFollowPolicy 的策略走 DecideNoFollow）。rm/mv 类入口专用。
func (e *Env) CheckPolicyUnlink(op, abs string, write bool) error {
	if e.Policy == nil {
		return nil
	}
	rd, wr := 0, 0
	if p, ok := e.Policy.(NoFollowPolicy); ok {
		rd, wr = p.DecideNoFollow(abs)
	} else {
		rd, wr = e.Policy.Decide(abs)
	}
	return e.checkGrades(op, abs, rd, wr, write)
}

// checkGrades 是两种路径策略门的共用判定：读 0 级 ⇔ deny 命中（读默认开放），
// deny 读写双拒、grant/审批不可绕过；其余拒绝（白名单外写 = 写 0 级、等级不够）
// 可经 grant/审批放行。
func (e *Env) checkGrades(op, abs string, rd, wr int, write bool) error {
	need := rd
	if write {
		need = wr
	}
	if rd == 0 {
		return &proto.DeniedError{Reason: fmt.Sprintf("%s: %s is in the fs_deny list and cannot be granted (remove the deny through local management)", op, abs)}
	}
	if need == 0 || e.Granted < need {
		return &proto.DeniedError{Reason: fmt.Sprintf("%s: %s is not allowed by host file policy; request access with exec grant fs %s --temp", op, abs, abs)}
	}
	return nil
}

// Resolve 按 §2.1.1 可解析层展开指令路径参数（proto.ResolvePath 唯一实现）。
func (e *Env) Resolve(p string) (string, error) {
	return proto.ResolvePath(p, e.Workdir, e.Vars)
}

// CheckPath 校验展开后的绝对路径位于 Roots 内（Roots 为空 = 不限制）。
// 所有指令在 Resolve 之后、访问 VFS 之前必须调用——执行层强制收容，
// 防止路径穿越逃出可访问根（§2.1.1：规则匹配层与执行层独立计算、结果一致）。
// 返回 *proto.ExecError（Denied 语义，不可审批绕过）。
func (e *Env) CheckPath(action, abs string) error {
	if len(e.Roots) == 0 {
		return nil
	}
	for _, r := range e.Roots {
		// 根为 "/" 时任何绝对路径均在根内（chroot 单根语义）
		if r == "/" {
			if strings.HasPrefix(abs, "/") {
				return nil
			}
			continue
		}
		if abs == r || strings.HasPrefix(abs, r+"/") {
			return nil
		}
	}
	return &proto.ExecError{Action: action, Reason: fmt.Sprintf("path outside allowed roots: %s", abs)}
}

// HTTPReq 描述一次 curl 请求（方法/URL/头/体；GET 默认，无体方法 Body 为空）。
// 头键已归一为 HTTP 规范形态（如 Content-Type）。
type HTTPReq struct {
	Method  string
	URL     string
	Headers map[string]string // nil=无自定义头
	Body    []byte            // GET/HEAD 等无体方法为空
}

// Fetcher 是 curl 的 HTTP 获取接口。实现方负责重定向与 SSRF 等策略
// （cloud 严格 / 物理 host 不限制，§5.4）。
// 返回 size 为响应总字节数（http Content-Length），未知返回 -1（超限报错消息用）。
type Fetcher interface {
	Fetch(ctx context.Context, req HTTPReq) (body io.ReadCloser, size int64, err error)
}

// FetchFunc 适配函数为 Fetcher。
type FetchFunc func(ctx context.Context, req HTTPReq) (io.ReadCloser, int64, error)

// Fetch 实现 Fetcher。
func (f FetchFunc) Fetch(ctx context.Context, req HTTPReq) (io.ReadCloser, int64, error) {
	return f(ctx, req)
}
