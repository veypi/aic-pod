// Execute 是 exec 接入层的执行编排（hosts-vsh-redesign §2.4）：
// 前台等待 W，等待到期按通道策略二选一——登记后台（NATS/AI：bg 机制服务
// AI 跨 tool call 管理）或返回 ErrWaitElapsed（RTC：超时即超时，执行继续、
// 不产生 bg 记录）。
//
// 分层（2026-09-28 用户裁定）：vsh 引擎是业务无关的 bash——执行 script、
// 返回 stdout/stderr/exit_code；本包只做执行编排机制（等待/转后台/墙钟），
// 不做任何输出 shaping（预览截断/attrs/日志文件归各接入层）。
//
// 记录模型（2026-10-06 §5）：建立完整记录（cancel 已绑定）→ 发布记录 →
// 启动脚本。前台完成移除记录；NATS 等待超时同一记录标记 bg；RTC 等待
// 超时保留前台记录。取消（cancel/bg kill/断连）查任务表同一记录。
package execution

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ErrWaitElapsed 前台等待到期但未登记后台（adoptOnTimeout=false——RTC 直连
// 语义：超时回报调用方，执行继续、不产生 bg 记录，取消经 cancel/记录）。
var ErrWaitElapsed = errors.New("exec: foreground wait elapsed; execution continues")

// ErrCapacity 标记转后台登记容量不足（任务表满）：本次执行已被取消（不让
// 未登记的执行游离运行），调用方据此返回资源类错误码（overloaded）并保留
// 已创建的日志地址（hosts-vsh-redesign §2.4）。
var ErrCapacity = errors.New("exec: background capacity exceeded")

// Outcome 是编排一次执行的结果。
type Outcome struct {
	// Run 是本次执行的唯一运行记录（任务表持有；不是副本）——调用方
	// 等待结束后执行仍在运行时，取消/查询经任务表（CancelByRequest/
	// CancelConnection），Result/Done 仅供收尾同步。
	Run        *Run
	Result     *ExecResult
	Err        error
	Background bool // 前台等待到期，已登记后台
	Task       Task // Background=true 时的登记快照
}

// Execute 运行脚本（后台墙钟）并前台等待 wait；到期仍未完成时：
//   - adoptOnTimeout=true（NATS/AI）：把同一记录标记 bg（不重启、不重放、
//     不换日志），容量不足则取消本次执行并返回容量错误；
//   - adoptOnTimeout=false（RTC）：返回 ErrWaitElapsed，执行继续。
//
// req.Timeout 指定执行墙钟（默认 30min）。onDone 在执行结束、记录结算前
// 调用一次；调用方在其中关闭日志等资源，前台等待结束不触发资源回收。
func Execute(ctx context.Context, eng *Engine, req ExecRequest, wait time.Duration, meta TaskMeta, adoptOnTimeout bool, onDone func(*ExecResult)) *Outcome {
	if wait <= 0 || wait > MaxForegroundWait {
		wait = MaxForegroundWait
	}
	// 运行 ctx 独立于请求 ctx：传输断线/前台等待结束都不取消执行（§2.6），
	// 取消唯一来源是 cancel/bg kill（记录）或墙钟到期。
	if req.Timeout <= 0 {
		req.Timeout = BackgroundWallClock
	}
	runCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), req.Timeout)
	// 建立完整记录（cancel 已绑定）→ 发布 → 启动脚本。
	r := eng.Tasks.register(cancel, req.Script, meta)
	req.WaitBudget = wait
	runCtx = context.WithValue(runCtx, execRunKey{}, r)
	go func() {
		defer cancel()
		res, err := eng.Exec(runCtx, req)
		if runCtx.Err() == context.DeadlineExceeded {
			// 墙钟到期：退出码 124（结果可能为 nil）
			if res == nil {
				res = &ExecResult{ExitCode: 124}
			} else {
				res.ExitCode = 124
			}
		}
		if onDone != nil {
			onDone(res)
		}
		eng.Tasks.finish(r, res, err)
	}()
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-r.Done():
		res, err := r.Result()
		return &Outcome{Run: r, Result: res, Err: err}
	case <-ctx.Done():
		// 调用方 ctx 结束（传输断连/预算到期）：结束等待但不取消执行、
		// 不转后台（§2.6：断线不是取消）——执行 goroutine 继续跑完写
		// 日志，取消唯一来源仍是 cancel/bg kill（记录）或墙钟到期。
		return &Outcome{Run: r, Err: ctx.Err()}
	case <-timer.C:
	}
	if !adoptOnTimeout {
		return &Outcome{Run: r, Err: ErrWaitElapsed}
	}
	// 等待到期：同一记录标记 bg（完成与超时的竞争在记录锁内裁决——
	// 已完成则直接返回完成结果，不产生 bg 记录）。
	task, err := eng.Tasks.adoptBackground(r)
	if err != nil {
		if errors.Is(err, errRunFinished) {
			res, rerr := r.Result()
			return &Outcome{Run: r, Result: res, Err: rerr}
		}
		// 容量不足：取消本次执行，不让未登记的执行继续运行。取消是异步
		// 的——执行 goroutine 仍在收尾写日志，日志关闭由调用方在记录
		// 实际结束时完成（与转后台路径一致）。
		r.Cancel()
		res, _ := r.Result()
		return &Outcome{Run: r, Result: res, Err: fmt.Errorf("%w: %v", ErrCapacity, err)}
	}
	return &Outcome{Run: r, Background: true, Task: task}
}
