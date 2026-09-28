// Package execwait 是 exec 接入层的执行编排（hosts-vsh-redesign §2.4）：
// 前台等待 W，等待到期按通道策略二选一——登记后台（NATS/AI：bg 机制服务
// AI 跨 tool call 管理）或返回 ErrWaitElapsed（RTC：超时即超时，执行继续、
// 不产生 bg 记录）。
//
// 分层（2026-09-28 用户裁定）：vsh 引擎是业务无关的 bash——执行 script、
// 返回 stdout/stderr/exit_code；本包只做执行编排机制（等待/Adopt/墙钟），
// 不做任何输出 shaping（预览截断/attrs/日志文件归各接入层）。
package execwait

import (
	"context"
	"errors"
	"fmt"
	"time"

	vshglue "github.com/veypi/aic-pod/libs/vsh"
)

// ErrWaitElapsed 前台等待到期但未登记后台（adoptOnTimeout=false——RTC 直连
// 语义：超时回报调用方，执行继续、不产生 bg 记录，取消经 cancel/句柄）。
var ErrWaitElapsed = errors.New("exec: foreground wait elapsed; execution continues")

// ErrCapacity 标记转后台登记容量不足（任务表满）：本次执行已被取消（不让
// 未登记的执行游离运行），调用方据此返回资源类错误码（overloaded）并保留
// 已创建的日志地址（hosts-vsh-redesign §2.4）。
var ErrCapacity = errors.New("exec: background capacity exceeded")

// Outcome 是编排一次执行的结果。
type Outcome struct {
	Result     *vshglue.ExecResult
	Err        error
	Background bool         // 前台等待到期，已登记后台
	Task       vshglue.Task // Background=true 时的登记快照
}

// Execute 运行脚本（后台墙钟）并前台等待 wait；到期仍未完成时：
//   - adoptOnTimeout=true（NATS/AI）：把同一运行登记进任务表（不重启、不
//     重放、不换日志），容量不足则取消本次执行并返回容量错误；
//   - adoptOnTimeout=false（RTC）：返回 ErrWaitElapsed，执行继续。
//
// req.Timeout/WaitBudget 由本包统一设置（墙钟/前台预算）；req.Stdout/
// Stderr 接的 writer（日志文件）由调用方创建持有，本包不负责关闭——
// 日志在实际执行结束时由调用方 goroutine 关闭。
func Execute(ctx context.Context, eng *vshglue.Engine, req vshglue.ExecRequest, wait time.Duration, meta vshglue.TaskMeta, adoptOnTimeout bool) *Outcome {
	if wait <= 0 || wait > vshglue.MaxForegroundWait {
		wait = vshglue.MaxForegroundWait
	}
	// 运行 ctx 独立于请求 ctx：传输断线/前台等待结束都不取消执行（§2.6），
	// 取消唯一来源是 cancel/bg kill（句柄）或墙钟到期。
	runCtx, cancel := context.WithTimeout(context.Background(), vshglue.BackgroundWallClock)
	// 外层可预建句柄（host 取消登记表按 request_id 持有同一句柄——cancel
	// 动作与 bg kill 终止的是同一执行）。
	h := req.Handle
	if h == nil {
		h = vshglue.NewExecHandle(cancel)
	} else {
		h.BindCancel(cancel)
	}
	req.Handle = h
	req.WaitBudget = wait
	req.Timeout = vshglue.BackgroundWallClock
	go func() {
		res, err := eng.Exec(runCtx, req)
		if runCtx.Err() == context.DeadlineExceeded {
			// 墙钟到期：退出码 124（结果可能为 nil）
			if res == nil {
				res = &vshglue.ExecResult{ExitCode: 124}
			} else {
				res.ExitCode = 124
			}
			h.Finish(res, err)
			return
		}
		h.Finish(res, err)
	}()
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-h.Done():
		res, err := h.Result()
		return &Outcome{Result: res, Err: err}
	case <-ctx.Done():
		// 调用方 ctx 结束（传输断连/预算到期）：结束等待但不取消执行、
		// 不登记后台（§2.6：断线不是取消）——执行 goroutine 继续跑完写
		// 日志，取消唯一来源仍是 cancel/bg kill（句柄）或墙钟到期。
		return &Outcome{Err: ctx.Err()}
	case <-timer.C:
	}
	if !adoptOnTimeout {
		return &Outcome{Err: ErrWaitElapsed}
	}
	// 等待到期：登记后台（完成与超时的竞争由 Adopt 原子裁决——已完成则
	// 直接返回完成结果，不产生 bg 记录）。
	task, err := eng.Tasks.Adopt(h, req.Script, meta)
	if err != nil {
		if h.DoneFlag() {
			// 竞争：登记时执行已完成——返回完成结果，不留 bg 记录
			res, rerr := h.Result()
			return &Outcome{Result: res, Err: rerr}
		}
		// 容量不足：取消本次执行，不让未登记的执行继续运行。取消是异步
		// 的——执行 goroutine 仍在收尾写日志，日志关闭由调用方在句柄
		// 实际结束时完成（与转后台路径一致）。
		h.Cancel()
		res, _ := h.Result()
		return &Outcome{Result: res, Err: fmt.Errorf("%w: %v", ErrCapacity, err)}
	}
	return &Outcome{Background: true, Task: task}
}
