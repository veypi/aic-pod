package execution

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// --- 唯一运行记录表（四仓精简 §5，2026-10-06）：前台与后台是一次执行的
// 两个展示阶段。建立完整记录（cancel 已绑定）→ 发布 → 启动脚本；
// 前台完成移除记录，NATS 等待超时同一记录标记 bg 并分配 bgID，RTC 等待
// 超时保留前台记录，后台完成保留有界历史。owner/session/requestID/
// connectionID、日志地址、取消函数和最终结果只存一份；bg 列表是记录的
// 筛选/快照。cancel(requestID)、bg kill、断连都查此表；完成、转后台和
// 取消的竞争在记录锁内裁决。容量闸只在转后台时检查（前台执行不计入 bg
// 配额）。host 不再有第二张前台登记表。 ---

// TaskMeta 是运行登记的元信息（归属 + 请求/连接关联 + 日志路径）。
type TaskMeta struct {
	// Owner/Session 归属：list/wait/kill/cancel 按 (user, session) 检查。
	Owner, Session string
	// RequestID 请求关联（cancel 按 request_id 找到同一执行）。
	RequestID string
	// ConnectionID 传输连接关联（RTC 断连只取消该连接的前台执行）。
	ConnectionID string
	// LogOut/LogErr stdout/stderr 日志地址（对应端可读取路径）。
	LogOut, LogErr string
}

// ErrNoRun 是 cancel 的「无此活跃执行」哨兵（归属不匹配等同不存在）。
var ErrNoRun = errors.New("no active execution")

// errRunFinished 转后台竞争回退：登记时执行已完成（不产生 bg 记录）。
var errRunFinished = errors.New("execution already finished")

// Run 是一次执行的唯一运行记录（任务表持有；Execute 经 Outcome.Run 透出
// 同一记录——不是副本）。身份字段登记后不可变；运行态由记录锁裁决。
type Run struct {
	mu sync.Mutex
	// 登记时固定：
	key          string // 表内主键（= RequestID；空则内部生成）
	command      string
	owner        string
	session      string
	requestID    string
	connectionID string
	logOut       string
	logErr       string
	startedAt    time.Time
	seq          int
	// 运行态（mu 守卫；完成/转后台/取消在此裁决）：
	cancel     context.CancelFunc
	done       chan struct{}
	res        *ExecResult
	err        error
	settled    bool // 终态已结算（只发生一次）
	killed     bool // kill 已受理（130 语义，覆盖自然退出码）
	background bool
	bgID       string
	bgSeq      int
	finishedAt time.Time
}

// Done 完成通知（执行真正结束、日志等收尾完成后关闭）。
func (r *Run) Done() <-chan struct{} { return r.done }

// Result 取最终结果（未完成返回 nil, nil）。
func (r *Run) Result() (*ExecResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.res, r.err
}

// Cancel 终止本次执行（脚本及受管子进程）。cancel 在登记发布前已绑定，
// 不存在「取消先于绑定」的竞态窗。
func (r *Run) Cancel() {
	r.mu.Lock()
	cancel := r.cancel
	r.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// BGID 当前 bg 编号（未转后台为空；bg wait 禁止等自身用）。
func (r *Run) BGID() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.bgID
}

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

// TaskTable 唯一运行记录表（不启动执行、不存输出缓冲）。容量闸：全局
// max(2, NumCPU/2) + per-owner 4 同时运行的 bg 上限，超额快速拒绝。
// 完成记录有界保留（仅 bg），不做持久任务恢复。
type TaskTable struct {
	mu          sync.Mutex
	runs        map[string]*Run // 全部运行记录（前台 + 后台 + bg 历史）
	byBG        map[string]*Run // bgID 索引（转后台时建立）
	seq         int             // 内部主键序号
	bgSeq       int             // bgID 序号（连续；逐出按此定新旧）
	bgRunning   int
	bgByOwner   map[string]int
	maxGlobal   int
	maxPerOwner int
	maxRetained int
}

func NewTaskTable() *TaskTable {
	return NewTaskTableWithCaps(maxRunningTasksGlobal(), MaxRunningTasksPerOwner)
}

// NewTaskTableWithCaps 自定义容量闸（测试用；生产走 NewTaskTable）。
func NewTaskTableWithCaps(maxGlobal, maxPerOwner int) *TaskTable {
	return &TaskTable{
		runs:        map[string]*Run{},
		byBG:        map[string]*Run{},
		bgByOwner:   map[string]int{},
		maxGlobal:   maxGlobal,
		maxPerOwner: maxPerOwner,
		maxRetained: MaxRetainedFinishedTasks,
	}
}

// register 建立完整记录（cancel 已绑定）并发布——先于脚本启动。
func (t *TaskTable) register(cancel context.CancelFunc, command string, meta TaskMeta) *Run {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.seq++
	key := meta.RequestID
	if key == "" {
		key = fmt.Sprintf("run-%d", t.seq)
	}
	r := &Run{
		key: key, command: command,
		owner: meta.Owner, session: meta.Session,
		requestID: meta.RequestID, connectionID: meta.ConnectionID,
		logOut: meta.LogOut, logErr: meta.LogErr,
		startedAt: time.Now(), seq: t.seq,
		cancel: cancel, done: make(chan struct{}),
	}
	// requestID 由协议保证唯一（NATS/RTC 请求 ID、cloud tool call ID）；
	// 撞键时旧记录仍可经 bgID/连接索引到达，这里直接覆盖主键。
	t.runs[key] = r
	return r
}

// finish 结算终态（执行 goroutine 调用一次）：终态在记录锁内立即结算，
// 前台记录移除，bg 记录保留（容量同步释放）并触发有界清理。
func (t *TaskTable) finish(r *Run, res *ExecResult, err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	r.mu.Lock()
	if r.settled {
		r.mu.Unlock()
		return
	}
	r.res, r.err = res, err
	r.settled = true
	r.finishedAt = time.Now()
	bg := r.background
	if bg {
		t.bgRunning--
		t.bgByOwner[r.owner]--
	}
	close(r.done)
	r.mu.Unlock()
	if !bg {
		// 前台完成：收尾后移除记录。
		if t.runs[r.key] == r {
			delete(t.runs, r.key)
		}
		return
	}
	t.evictLocked()
}

// adoptBackground 等待超时转后台：同一记录标记 bg 并分配 bgID（不重启、
// 不重放、不换日志）。已完成返回 errRunFinished（调用方回退为完成结果）；
// 容量不足返回配额错误（调用方取消本次执行）。竞争在记录锁内裁决。
func (t *TaskTable) adoptBackground(r *Run) (Task, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.evictLocked()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.settled {
		return Task{}, errRunFinished
	}
	if t.bgRunning >= t.maxGlobal {
		return Task{}, fmt.Errorf("task table full（全局运行上限 %d，当前 %d）——先 bg list 查看、bg kill 释放或稍后重试", t.maxGlobal, t.bgRunning)
	}
	if t.bgByOwner[r.owner] >= t.maxPerOwner {
		return Task{}, fmt.Errorf("task table full（单归属运行上限 %d，当前 %d）——先 bg list 查看、bg kill 释放或稍后重试", t.maxPerOwner, t.bgByOwner[r.owner])
	}
	t.bgSeq++
	r.background = true
	r.bgID = fmt.Sprintf("bg-%d", t.bgSeq)
	r.bgSeq = t.bgSeq
	t.byBG[r.bgID] = r
	t.bgRunning++
	t.bgByOwner[r.owner]++
	return snapshotLocked(r), nil
}

// evictLocked 逐出最旧的已完成 bg 记录至保留上限（t.mu 已持有）。
func (t *TaskTable) evictLocked() {
	for {
		var oldest *Run
		finished := 0
		for _, r := range t.byBG {
			r.mu.Lock()
			settled := r.settled
			r.mu.Unlock()
			if !settled {
				continue
			}
			finished++
			if oldest == nil || r.bgSeq < oldest.bgSeq {
				oldest = r
			}
		}
		if finished <= t.maxRetained || oldest == nil {
			return
		}
		delete(t.byBG, oldest.bgID)
		if t.runs[oldest.key] == oldest {
			delete(t.runs, oldest.key)
		}
	}
}

// statusLocked 派生展示状态（r.mu 已持有）：killed 恒优先（kill 时进程
// 可能仍正常退出 0）。
func statusLocked(r *Run) (status string, exit int, errStr string) {
	if r.killed {
		return "killed", 130, ""
	}
	if !r.settled {
		return "running", 0, ""
	}
	exitCode := 0
	if r.res != nil {
		exitCode = r.res.ExitCode
	}
	switch {
	case r.err != nil:
		return "error", exitCode, r.err.Error()
	case r.res != nil && r.res.ExitCode == 124:
		return "timeout", 124, ""
	default:
		return "done", exitCode, ""
	}
}

// snapshotLocked 取记录快照（r.mu 已持有）。
func snapshotLocked(r *Run) Task {
	status, exit, errStr := statusLocked(r)
	return Task{
		ID: r.bgID, Command: r.command, Status: status, ExitCode: exit,
		StartedAt: r.startedAt, FinishedAt: r.finishedAt, Err: errStr,
		Owner: r.owner, Session: r.session, RequestID: r.requestID,
		LogOut: r.logOut, LogErr: r.logErr,
	}
}

func (r *Run) matches(owner, session string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.owner == owner && r.session == session
}

// List 按登记序返回该归属的 bg 任务快照（含保留的完成历史）。
func (t *TaskTable) List(owner, session string) []Task {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]Task, 0, len(t.byBG))
	for _, r := range t.byBG {
		if r.matches(owner, session) {
			r.mu.Lock()
			out = append(out, snapshotLocked(r))
			r.mu.Unlock()
		}
	}
	return out
}

// Get 取单个 bg 任务快照（归属不匹配等同不存在）。
func (t *TaskTable) Get(id, owner, session string) (Task, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	r, ok := t.byBG[id]
	if !ok || !r.matches(owner, session) {
		return Task{}, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return snapshotLocked(r), true
}

// Wait 等待 bg 任务结束（d≤0 不等待，立即返回当前快照；归属不匹配等同不存在）。
func (t *TaskTable) Wait(ctx context.Context, id string, d time.Duration, owner, session string) (Task, error) {
	t.mu.Lock()
	r, ok := t.byBG[id]
	if !ok || !r.matches(owner, session) {
		t.mu.Unlock()
		return Task{}, fmt.Errorf("bg: no such task %q", id)
	}
	t.mu.Unlock()
	if d > 0 {
		timer := time.NewTimer(d)
		defer timer.Stop()
		select {
		case <-r.done:
		case <-ctx.Done():
			r.mu.Lock()
			defer r.mu.Unlock()
			return snapshotLocked(r), ctx.Err()
		case <-timer.C:
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return snapshotLocked(r), nil
}

// Kill 请求终止 bg 任务：置 killed（快照即时可见 = kill 已受理）并取消
// 同一执行；终态与容量释放在执行实际结束时结算（bg wait 等 Done，天然
// 等到真正结束）。归属不匹配等同不存在。
func (t *TaskTable) Kill(id, owner, session string) error {
	t.mu.Lock()
	r, ok := t.byBG[id]
	if !ok || !r.matches(owner, session) {
		t.mu.Unlock()
		return fmt.Errorf("bg: no such task %q", id)
	}
	r.mu.Lock()
	if r.settled {
		status, _, _ := statusLocked(r)
		r.mu.Unlock()
		t.mu.Unlock()
		return fmt.Errorf("bg: task %q already finished (%s)", id, status)
	}
	r.killed = true
	cancel := r.cancel
	r.mu.Unlock()
	t.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return nil
}

// CancelByRequest 按 request_id 取消执行（cancel 动作唯一查询路径；
// 前台与 bg 同表）。bg 记录置 killed（130 语义）；归属不匹配或已终结
// 等同不存在。
func (t *TaskTable) CancelByRequest(requestID, owner, session string) error {
	t.mu.Lock()
	r, ok := t.runs[requestID]
	if !ok || r.owner != owner || r.session != session {
		t.mu.Unlock()
		return ErrNoRun
	}
	r.mu.Lock()
	if r.settled {
		r.mu.Unlock()
		t.mu.Unlock()
		return ErrNoRun
	}
	if r.background {
		r.killed = true
	}
	cancel := r.cancel
	r.mu.Unlock()
	t.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return nil
}

// CancelConnection 断连清理：取消该连接发起的前台执行（bg 任务继续；
// 与转后台的竞争在记录锁内裁决——已标记 bg 的记录不再取消）。
func (t *TaskTable) CancelConnection(connectionID string) {
	if connectionID == "" {
		return
	}
	t.mu.Lock()
	var cancels []context.CancelFunc
	for _, r := range t.runs {
		if r.connectionID != connectionID {
			continue
		}
		r.mu.Lock()
		if !r.settled && !r.background {
			cancels = append(cancels, r.cancel)
		}
		r.mu.Unlock()
	}
	t.mu.Unlock()
	for _, cancel := range cancels {
		if cancel != nil {
			cancel()
		}
	}
}

// WaitRun 等待 requestID 对应执行实际结束（测试与调用方收尾用——取消/
// 断连是异步的，执行 goroutine 写完日志才关闭 Done）。记录不存在 =
// 已终结。超时返回 false。
func (t *TaskTable) WaitRun(requestID string, d time.Duration) bool {
	t.mu.Lock()
	r := t.runs[requestID]
	t.mu.Unlock()
	if r == nil {
		return true
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-r.done:
		return true
	case <-timer.C:
		return false
	}
}
