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
	VFS ufs.FS
	Workdir string            // 当次调用显式携带的基准目录（缺省值由调用方先行填充，§2.1.1）
	Vars    map[string]string // 根变量映射（预留；三端当前均无变量）；nil = 物理 host
	// Roots 是可访问根列表（§2.1.1 执行层收容）：展开后的路径必须位于某个 root 内
	// （含 root 自身），否则 DeniedError（不可审批绕过）。
	// cloud = ["/"]（VFS chroot 到会话空间）；nil = 物理 host 不限制（用户机器自身边界）。
	Roots []string
	// ProtectRoots 是 rm/mv 的根目录硬保护列表（§5.4：cloud 会话空间根 / 物理 host 文件系统根）。
	// 命中返回 DeniedError，不可审批绕过。
	ProtectRoots []string
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

// Fetcher 是 curl 的 HTTP 获取接口。实现方负责重定向与 SSRF 等策略
// （cloud 严格 / 物理 host 不限制，§5.4）。
// 返回 size 为响应总字节数（http Content-Length），未知返回 -1（超限报错消息用）。
type Fetcher interface {
	Get(ctx context.Context, rawurl string) (body io.ReadCloser, size int64, err error)
}

// FetchFunc 适配函数为 Fetcher。
type FetchFunc func(ctx context.Context, rawurl string) (io.ReadCloser, int64, error)

// Get 实现 Fetcher。
func (f FetchFunc) Get(ctx context.Context, rawurl string) (io.ReadCloser, int64, error) {
	return f(ctx, rawurl)
}
