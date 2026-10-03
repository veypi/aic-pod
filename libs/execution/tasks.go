package execution

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// --- wait.go owns foreground waiting; the engine executes the script and
// 返回 stdout/stderr/exit_code；前台等待、超时转后台、日志与响应 shaping
// 都是调用方职责（§2.4/§2.5，2026-09-28 用户裁定重分层）。 ---

// TaskMeta 是转后台登记的元信息（日志路径 + 请求关联）。
type TaskMeta struct {
	// Owner/Session 归属：list/wait/kill/cancel 按 (user, session) 检查。
	Owner, Session string
	// RequestID 请求关联（cancel 按 request_id 找到同一执行）。
	RequestID string
	// LogOut/LogErr stdout/stderr 日志地址（对应端可读取路径）。
	LogOut, LogErr string
}

// --- 后台任务登记表（bg list/wait/kill；唯一来源 = exec 等待超时 Adopt） ---

// Task 是后台任务的快照。
type Task struct {
	ID         string
	Command    string
	Status     string // running / done / timeout / killed / error
	ExitCode   int
	StartedAt  time.Time
	FinishedAt time.Time
	Err        string
	// Owner/Session 归属（user_id, session_id）；RequestID 请求关联（cancel）。
	Owner, Session string
	RequestID      string
	// LogOut/LogErr 是 stdout/stderr 日志地址（bg list/wait 报告用；
	// 输出读取经 FS/cat，任务表不另存输出缓冲）。
	LogOut, LogErr string
}

// ExecHandle 是一次执行的外层句柄：前台等待、转后台登记与取消共用。
type ExecHandle struct {
	cancel context.CancelFunc
	// cancelRequested 记录「取消已请求但 cancel 尚未绑定」——Cancel 先于
	// BindCancel 到达时不丢失（接入层预建句柄与编排层绑定之间存在竞态窗），
	// BindCancel 绑定时补触发。
	cancelRequested bool
	done            chan struct{}
	mu              sync.Mutex
	res             *ExecResult
	err             error
	taskID          string
}

func NewExecHandle(cancel context.CancelFunc) *ExecHandle {
	return &ExecHandle{cancel: cancel, done: make(chan struct{})}
}

// Done 完成通知（执行结束关闭）。
func (h *ExecHandle) Done() <-chan struct{} { return h.done }

// DoneFlag 非阻塞完成查询。
func (h *ExecHandle) DoneFlag() bool {
	select {
	case <-h.done:
		return true
	default:
		return false
	}
}

// Finish 写入最终结果并关闭完成通知（只生效一次）。
func (h *ExecHandle) Finish(res *ExecResult, err error) {
	h.mu.Lock()
	select {
	case <-h.done:
		h.mu.Unlock()
		return
	default:
	}
	h.res, h.err = res, err
	close(h.done)
	h.mu.Unlock()
}

// Result 取最终结果（未完成返回 nil, nil）。
func (h *ExecHandle) Result() (*ExecResult, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.res, h.err
}

// Cancel 终止本次执行（脚本及受管子进程）。与 BindCancel 并发安全：
// cancel(request_id)/DisconnectTools 可能与 wait 编排层绑定墙钟
// cancel 并发，必须经锁读取（否则会读到半构造的 cancel 或漏取）。
// cancel 尚未绑定时请求不丢失：记录标志，由 BindCancel 补触发。
func (h *ExecHandle) Cancel() {
	h.mu.Lock()
	h.cancelRequested = true
	cancel := h.cancel
	h.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// BindCancel 绑定取消函数（接入层编排 wait 注入墙钟 ctx 的 cancel——
// 外层预建句柄后由编排层接管执行期限）。绑定前已有取消请求的立即补触发。
func (h *ExecHandle) BindCancel(cancel context.CancelFunc) {
	h.mu.Lock()
	h.cancel = cancel
	pending := h.cancelRequested
	h.mu.Unlock()
	if pending {
		cancel()
	}
}

// TaskID 转后台后分配的任务 ID（未登记为空）。
func (h *ExecHandle) TaskID() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.taskID
}

// adopt 登记任务 ID；已完成或未运行返回 false（调用方裁决竞争）。
func (h *ExecHandle) adopt(id string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	select {
	case <-h.done:
		return false
	default:
	}
	if h.taskID != "" {
		return false
	}
	h.taskID = id
	return true
}

type taskEntry struct {
	mu       sync.Mutex
	task     Task
	handle   *ExecHandle
	finished bool
	seq      int
	// released 容量计数是否已释放（mu 守卫）——只在 finalizeLocked 实际
	// 终结时释放一次（Kill 不再同步释放：容量反映真实在跑数量）。
	released bool
}

// TaskTable 后台任务登记表（exec 超时转后台的唯一来源；不启动执行、不存
// 输出缓冲）。容量闸：全局 max(2, NumCPU/2) + per-owner 4 同时运行上限，
// 超额快速拒绝。沿用完成记录的有界清理，不做持久任务恢复。
type TaskTable struct {
	mu             sync.Mutex
	tasks          map[string]*taskEntry
	seq            int
	running        int
	runningByOwner map[string]int
	maxGlobal      int
	maxPerOwner    int
	maxRetained    int
}

func NewTaskTable() *TaskTable {
	return NewTaskTableWithCaps(maxRunningTasksGlobal(), MaxRunningTasksPerOwner)
}

// NewTaskTableWithCaps 自定义容量闸（测试用；生产走 NewTaskTable）。
func NewTaskTableWithCaps(maxGlobal, maxPerOwner int) *TaskTable {
	return &TaskTable{
		tasks:          map[string]*taskEntry{},
		runningByOwner: map[string]int{},
		maxGlobal:      maxGlobal,
		maxPerOwner:    maxPerOwner,
		maxRetained:    MaxRetainedFinishedTasks,
	}
}

// Adopt 把一次仍在运行的执行登记为后台任务：分配 ID、容量计数；完成监视
// 从 handle 终结状态回填（不重启、不重放、不更换日志）。已完成或已登记的
// 句柄返回错误（exec 外层据此回退为完成结果——竞争只产生一个结果）。
func (t *TaskTable) Adopt(h *ExecHandle, command string, meta TaskMeta) (Task, error) {
	t.mu.Lock()
	// 容量检查前先惰性结算：已完成任务同步释放名额——否则满载时连
	// bg list/kill 都无法执行（任何新 exec 都被拒），形成死锁。
	t.evictFinishedLocked()
	if t.running >= t.maxGlobal {
		n := t.running
		t.mu.Unlock()
		return Task{}, fmt.Errorf("task table full（全局运行上限 %d，当前 %d）——先 bg list 查看、bg kill 释放或稍后重试", t.maxGlobal, n)
	}
	if t.runningByOwner[meta.Owner] >= t.maxPerOwner {
		n := t.runningByOwner[meta.Owner]
		t.mu.Unlock()
		return Task{}, fmt.Errorf("task table full（单归属运行上限 %d，当前 %d）——先 bg list 查看、bg kill 释放或稍后重试", t.maxPerOwner, n)
	}
	t.seq++
	id := fmt.Sprintf("bg-%d", t.seq)
	// 句柄登记（原子裁决完成/超时竞争，t.mu 持有期间调用——h.mu 与 t.mu
	// 无反向锁序）：失败 = 已完成/已登记，回退为完成结果由调用方处理。
	if !h.adopt(id) {
		t.mu.Unlock()
		return Task{}, fmt.Errorf("execution already finished")
	}
	entry := &taskEntry{handle: h, seq: t.seq}
	entry.task = Task{ID: id, Command: command, Status: "running", StartedAt: time.Now(),
		Owner: meta.Owner, Session: meta.Session, RequestID: meta.RequestID,
		LogOut: meta.LogOut, LogErr: meta.LogErr}
	t.tasks[id] = entry
	t.running++
	t.runningByOwner[meta.Owner]++
	t.evictFinishedLocked()
	t.mu.Unlock()
	// 终态惰性回填：读路径（List/Get/Wait/Kill/evict）经 finalizeLocked 从
	// 句柄结果结算——Wait 解除阻塞即终态可见，容量同步恢复（无 watcher
	// goroutine 的异步窗口）。
	return entry.snapshot(), nil
}

// finalizeLocked 从执行句柄结算终态（幂等；t.mu 已持有，锁序
// t.mu→entry.mu→handle.mu）。容量计数在实际终结时同步释放，只发生一次。
func (t *TaskTable) finalizeLocked(e *taskEntry) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.finished || !e.handle.DoneFlag() {
		return
	}
	res, err := e.handle.Result()
	e.task.FinishedAt = time.Now()
	switch {
	case e.task.Status == "killed":
		// Kill 已置位——130 不被执行结果覆盖（kill 时进程可能仍正常退出 0）。
	case err != nil:
		if res != nil {
			e.task.ExitCode = res.ExitCode
		}
		e.task.Status = "error"
		e.task.Err = err.Error()
	case res != nil && res.ExitCode == 124:
		e.task.ExitCode = 124
		e.task.Status = "timeout"
	default:
		if res != nil {
			e.task.ExitCode = res.ExitCode
		}
		e.task.Status = "done"
	}
	e.finished = true
	if !e.released {
		e.released = true
		t.running--
		t.runningByOwner[e.task.Owner]--
	}
}

// evictFinishedLocked 逐出最旧的已完成任务至保留上限（t.mu 已持有；
// 锁序与 List 一致 t.mu→entry.mu）。
func (t *TaskTable) evictFinishedLocked() {
	for {
		var oldest *taskEntry
		finished := 0
		for _, e := range t.tasks {
			t.finalizeLocked(e)
			e.mu.Lock()
			fin := e.finished
			e.mu.Unlock()
			if !fin {
				continue
			}
			finished++
			if oldest == nil || e.seq < oldest.seq {
				oldest = e
			}
		}
		if finished <= t.maxRetained || oldest == nil {
			return
		}
		delete(t.tasks, oldest.task.ID)
	}
}

// owned 归属检查：(owner, session) 二元组一致才可见/可管（跨会话默认隔离；
// 无会话的手动调用属独立 manual 范围——session 空只匹配空）。
func owned(e *taskEntry, owner, session string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.task.Owner == owner && e.task.Session == session
}

// snapshot 先惰性结算终态再取值（t.mu 由调用方持有）。
func (t *TaskTable) snapshot(e *taskEntry) Task {
	t.finalizeLocked(e)
	return e.snapshot()
}

// List 按登记序返回该归属的任务快照。
func (t *TaskTable) List(owner, session string) []Task {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]Task, 0, len(t.tasks))
	for _, e := range t.tasks {
		if owned(e, owner, session) {
			out = append(out, t.snapshot(e))
		}
	}
	return out
}

// Get 取单个任务快照（归属不匹配等同不存在）。
func (t *TaskTable) Get(id, owner, session string) (Task, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.tasks[id]
	if !ok || !owned(e, owner, session) {
		return Task{}, false
	}
	return t.snapshot(e), true
}

// FindByRequest 按 request_id 找任务（cancel 与 bg kill 共用取消句柄）。
func (t *TaskTable) FindByRequest(requestID, owner, session string) (Task, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, e := range t.tasks {
		t.finalizeLocked(e)
		e.mu.Lock()
		if e.task.RequestID == requestID && e.task.Owner == owner && e.task.Session == session {
			e.mu.Unlock()
			return e.snapshot(), true
		}
		e.mu.Unlock()
	}
	return Task{}, false
}

// Wait 等待任务结束（d≤0 不等待，立即返回当前快照；归属不匹配等同不存在）。
func (t *TaskTable) Wait(ctx context.Context, id string, d time.Duration, owner, session string) (Task, error) {
	t.mu.Lock()
	e, ok := t.tasks[id]
	if !ok || !owned(e, owner, session) {
		t.mu.Unlock()
		return Task{}, fmt.Errorf("bg: no such task %q", id)
	}
	t.finalizeLocked(e) // 已终结直接出终态，不进等待
	t.mu.Unlock()
	if d > 0 {
		timer := time.NewTimer(d)
		defer timer.Stop()
		select {
		case <-e.handle.Done():
		case <-ctx.Done():
			return t.snapshot(e), ctx.Err()
		case <-timer.C:
			t.mu.Lock()
			defer t.mu.Unlock()
			return t.snapshot(e), nil
		}
		t.mu.Lock()
		defer t.mu.Unlock()
		return t.snapshot(e), nil // 句柄终结：读取即结算终态
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.snapshot(e), nil
}

// Kill 请求终止任务：置 killed/130 并取消同一执行句柄（归属不匹配等同
// 不存在）。终态（finished/FinishedAt）与容量释放不在此结算——执行实际
// 结束时由 finalizeLocked 回填（§2.6：实际结束后才报终态；bg wait 等待
// 句柄 Done，天然等到真正结束）。killed 状态即时可见 = kill 请求已受理。
func (t *TaskTable) Kill(id, owner, session string) error {
	t.mu.Lock()
	e, ok := t.tasks[id]
	if !ok || !owned(e, owner, session) {
		t.mu.Unlock()
		return fmt.Errorf("bg: no such task %q", id)
	}
	t.finalizeLocked(e) // 已终结按终态报错
	e.mu.Lock()
	if e.finished {
		status := e.task.Status
		e.mu.Unlock()
		t.mu.Unlock()
		return fmt.Errorf("bg: task %q already finished (%s)", id, status)
	}
	e.task.Status = "killed"
	e.task.ExitCode = 130
	handle := e.handle
	e.mu.Unlock()
	if handle != nil {
		handle.Cancel()
	}
	t.mu.Unlock()
	return nil
}

func (e *taskEntry) snapshot() Task {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.task
}
