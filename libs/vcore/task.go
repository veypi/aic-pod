package vcore

import (
	"context"
	"io"
)

// TaskRunner 是托管任务运行器（§5.9 exec_procs 的抽象面）：
// curl 无 -o 等「输出进 content」的指令经任务托管获得统一语义——
// 输出落盘日志、请求超时自动后台化（Background=true + id）、
// 前 1000 行截断返回、bg_list/bg_wait/bg_kill 统一管理。
// 日志落盘路径由实现方按任务 ID 决定（cloud = 会话空间 .exec/，host = {tmp}/aic/{sid}/）。
type TaskRunner interface {
	StartTask(ctx context.Context, opts TaskOptions) (*TaskResult, error)
}

// TaskOptions 是 StartTask 的入参。Run 为任务体：输出写 out（日志文件），
// 返回 error 表示任务失败（同步路径原样返回；后台路径错误写入日志）。
type TaskOptions struct {
	ID      string                                 // 任务 ID（工具调用 msg_id）
	Command string                                 // 展示名（bg_list）
	Run     func(ctx context.Context, out io.Writer) error
}

// TaskResult 是任务统一返回（正常/超时转后台同源）。
type TaskResult struct {
	Content    string
	Lines      int
	Truncated  bool
	Background bool
	ID         string
	LogPath    string
}
