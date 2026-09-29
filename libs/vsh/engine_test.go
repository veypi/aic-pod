package vsh

import (
	"context"
	"io"
	stdfs "io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/veypi/vbox"
	"github.com/veypi/vigo/contrib/ufs"
	gbfs "github.com/veypi/vsh/fs"
)

// newTestEngine cloud 形态 Engine：UFS localFS backing + jail /u/u1，无规则表。
func newTestEngine(t *testing.T) *Engine {
	t.Helper()
	backing, err := ufs.NewLocalFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e, err := NewEngine(EngineConfig{
		// HOME=/u/{uid}、PATH 钉死（env 由平台每次注入）。
		BaseEnv: map[string]string{"HOME": "/u/u1", "PATH": "/usr/bin:/bin"},
		NewSessionFS: func(key string) (gbfs.FileSystem, string, error) {
			fsys, err := NewCloudFS(CloudFSConfig{UserRoot: "/u/u1", Backing: backing})
			return fsys, "/u/u1", err
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// blockHandle 构造阻塞中的执行句柄（Adopt 测试件）。
func blockHandle() (*ExecHandle, chan struct{}) {
	release := make(chan struct{})
	h := NewExecHandle(func() {})
	go func() { <-release; h.Finish(&ExecResult{ExitCode: 0}, nil) }()
	return h, release
}

// TestTaskTableCapacity 容量闸（全局 + per-owner 上限，超额快速拒绝——
// fan-out/多会话并发可占满全部核的防线）。Adopt 模型：登记即占容量。
func TestTaskTableCapacity(t *testing.T) {
	t.Parallel()
	tt := NewTaskTableWithCaps(3, 2)
	releases := map[string]chan struct{}{}
	adopt := func(owner string) (Task, error) {
		h, release := blockHandle()
		task, err := tt.Adopt(h, "t", TaskMeta{Owner: owner, Session: "s1"})
		if err == nil {
			releases[task.ID] = release
		}
		return task, err
	}
	// per-owner=2：o1 第 3 个拒。
	if _, err := adopt("o1"); err != nil {
		t.Fatal(err)
	}
	k2, err := adopt("o1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adopt("o1"); err == nil || !strings.Contains(err.Error(), "单归属") {
		t.Fatalf("per-owner 超额应拒: %v", err)
	}
	// o2 第 1 个过（此时 running=3 达全局），o2 第 2 个撞全局拒。
	if _, err := adopt("o2"); err != nil {
		t.Fatal(err)
	}
	if _, err := adopt("o2"); err == nil || !strings.Contains(err.Error(), "全局") {
		t.Fatalf("全局超额应拒: %v", err)
	}
	// kill 后执行实际结束（release）→ 容量恢复。
	if err := tt.Kill(k2.ID, "o1", "s1"); err != nil {
		t.Fatal(err)
	}
	close(releases[k2.ID])
	snap, err := tt.Wait(context.Background(), k2.ID, 3*time.Second, "o1", "s1")
	if err != nil {
		t.Fatal(err)
	}
	if snap.Status != "killed" || snap.ExitCode != 130 {
		t.Fatalf("killed 任务实际结束后应保持 killed/130: %+v", snap)
	}
	if _, err := adopt("o1"); err != nil {
		t.Fatalf("释放后应可登记: %v", err)
	}
}

// TestTaskTableKillSettlesOnCompletion kill 语义：killed 状态即时可见（kill
// 请求已受理），但终态与容量在执行实际结束后才结算（§2.6 实际结束后才报
// 终态；kill 后、执行未退出前容量仍反映真实在跑数量）。
func TestTaskTableKillSettlesOnCompletion(t *testing.T) {
	t.Parallel()
	tt := NewTaskTableWithCaps(2, 2)
	h1, release1 := blockHandle()
	k1, err := tt.Adopt(h1, "t", TaskMeta{Owner: "o1", Session: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	h2, release2 := blockHandle()
	if _, err := tt.Adopt(h2, "t", TaskMeta{Owner: "o1", Session: "s1"}); err != nil {
		t.Fatal(err)
	}
	defer close(release2)
	if err := tt.Kill(k1.ID, "o1", "s1"); err != nil {
		t.Fatal(err)
	}
	// killed 即时可见。
	snap, ok := tt.Get(k1.ID, "o1", "s1")
	if !ok || snap.Status != "killed" || snap.ExitCode != 130 {
		t.Fatalf("kill 后快照 = %+v ok=%v", snap, ok)
	}
	// 执行未实际退出：容量未释放，新登记仍撞满。
	h3, release3 := blockHandle()
	defer close(release3)
	if _, err := tt.Adopt(h3, "t", TaskMeta{Owner: "o1", Session: "s1"}); err == nil {
		t.Fatal("执行未退出时容量不应提前释放")
	}
	// 执行实际结束 → 终态结算 + 容量释放（不双重释放）。
	close(release1)
	if _, err := tt.Wait(context.Background(), k1.ID, 3*time.Second, "o1", "s1"); err != nil {
		t.Fatal(err)
	}
	if _, err := tt.Adopt(h3, "t", TaskMeta{Owner: "o1", Session: "s1"}); err != nil {
		t.Fatalf("实际结束后应可补位: %v", err)
	}
	tt.mu.Lock()
	defer tt.mu.Unlock()
	if tt.running != 2 || tt.runningByOwner["o1"] != 2 {
		t.Fatalf("running=%d byOwner=%v, want 2/2（双重释放？）", tt.running, tt.runningByOwner)
	}
}

// TestTaskTableEviction 完成任务表保留上限：超出逐出最旧。
func TestTaskTableEviction(t *testing.T) {
	t.Parallel()
	tt := NewTaskTableWithCaps(16, 16)
	tt.maxRetained = 3
	var last Task
	for i := 0; i < 6; i++ {
		h := NewExecHandle(func() {})
		h.Finish(&ExecResult{ExitCode: 0}, nil)
		// 已完成句柄不能 Adopt——先登记再完成
		h2, release := blockHandle()
		task, err := tt.Adopt(h2, "t", TaskMeta{Owner: "o1", Session: "s1"})
		if err != nil {
			t.Fatal(err)
		}
		close(release)
		if _, err := tt.Wait(context.Background(), task.ID, 3*time.Second, "o1", "s1"); err != nil {
			t.Fatal(err)
		}
		last = task
	}
	tasks := tt.List("o1", "s1")
	if len(tasks) > 4 { // maxRetained 3 + 最后一个 running/finished
		t.Fatalf("retained = %d, want <= 4", len(tasks))
	}
	// 最新的必须在，最旧的 bg-1/bg-2 已逐出。
	if _, ok := tt.Get(last.ID, "o1", "s1"); !ok {
		t.Fatalf("latest task %s evicted", last.ID)
	}
	if _, ok := tt.Get("bg-1", "o1", "s1"); ok {
		t.Fatal("oldest bg-1 should be evicted")
	}
}

// TestConcurrentExecsDoNotBlock 每次 exec 独立会话：一个长执行不阻塞同会话
// 另一执行（含 bg 组合脚本——不靠单指令特判，§2.4 救场要求）。
func TestConcurrentExecsDoNotBlock(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t)
	ctx := context.Background()
	// 长执行占着（旧模型会持会话锁）
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = e.Exec(ctx, ExecRequest{SessionKey: "s1", Owner: "u:t1", Script: "sleep 3", Timeout: BackgroundWallClock})
	}()
	defer func() { <-done }()
	time.Sleep(300 * time.Millisecond)
	start := time.Now()
	res, err := e.Exec(ctx, ExecRequest{SessionKey: "s1", Owner: "u:t1", Script: "bg list | grep -c running || true"})
	if err != nil {
		t.Fatal(err)
	}
	if el := time.Since(start); el > 2*time.Second {
		t.Fatalf("组合脚本被长执行阻塞 %v", el)
	}
	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
}

// TestEngineTrustedContext Exec 把身份与审批事实注入可信 ctx（不经 env）：
// 平台命令经 SessionFromContext 取会话；grant 修改走 grant_approved 门。
func TestEngineTrustedContext(t *testing.T) {
	t.Parallel()
	backing, err := ufs.NewLocalFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var gotSid string
	e, err := NewEngine(EngineConfig{
		BaseEnv: map[string]string{"HOME": "/u/u1", "PATH": "/usr/bin:/bin"},
		NewSessionFS: func(key string) (gbfs.FileSystem, string, error) {
			fsys, err := NewCloudFS(CloudFSConfig{UserRoot: "/u/u1", Backing: backing})
			return fsys, "/u/u1", err
		},
		Platform: PlatformDeps{
			Grant: func(ctx context.Context, sessionKey, domain, target string, permanent bool) (string, error) {
				gotSid = SessionFromContext(ctx)
				return "ok", nil
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	// 无批准事实：grant 被拒（permission_denied，规则不变）
	res, err := e.Exec(context.Background(), ExecRequest{SessionKey: "sid-42", Script: "grant fs /u/u1/x"})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode == 0 || !strings.Contains(res.Stderr, "permission_denied") {
		t.Fatalf("unapproved grant: exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	if gotSid != "" {
		t.Fatal("Grant called without grant_approved")
	}
	// env 伪造无效（脚本可修改的 AIC_VSH_* 不再携带授权）
	res, err = e.Exec(context.Background(), ExecRequest{SessionKey: "sid-42", Script: "AIC_VSH_LEVEL=9 grant fs /u/u1/x"})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode == 0 || !strings.Contains(res.Stderr, "permission_denied") {
		t.Fatalf("env-forged grant: exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	// 批准事实：执行；动态形态（变量拼接）也过同一入口
	res, err = e.Exec(context.Background(), ExecRequest{SessionKey: "sid-42", GrantApproved: true, Script: "g=gra; ${g}nt fs /u/u1/x"})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("approved dynamic grant: exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	if gotSid != "sid-42" {
		t.Fatalf("SessionFromContext = %q, want sid-42", gotSid)
	}
}

// TestCloudJailAncestorMeta 内建命令的祖先链走访（mkdir -p 逐级 Stat、cd/ls
// 符号链接解析）读 jail 根祖先（/、/u）放行元数据；界外内容读写、根列表仍
// 硬拒。mkdir 收尾 Chmod 在无权限位的 UFS backing 上 noop（不假失败）。
func TestCloudJailAncestorMeta(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t)
	type tc struct {
		script   string
		wantCode int
		wantOut  string // stdout 包含
		wantErr  string // stderr 包含
	}
	for _, c := range []tc{
		{"mkdir /u/u1/aa && echo ok", 0, "ok", ""},
		{"mkdir -p /u/u1/a/b/c && echo ok", 0, "ok", ""},
		{"cd /u/u1 && pwd", 0, "/u/u1", ""},
		{"cp -r /u/u1/a /u/u1/acopy && echo ok", 0, "ok", ""},
		{"ls /u/u1", 0, "acopy", ""},
		// /dev 虚拟设备：写丢弃、不到达适配器（静态预检同步豁免）。
		{"echo x > /dev/null && echo ok", 0, "ok", ""},
		{"ls /nonexistent 2>/dev/null; echo code=$?", 0, "code=2", ""},
		// /dev/stdout|stderr 是 runner fd 别名（per-exec 流），与 bash 一致。
		{"echo out > /dev/stdout && echo err > /dev/stderr", 0, "out", "err"},
		// 拒绝面（放宽不漏）：界外读/写、别人用户根、根列表。
		{"mkdir /u/other/x", 1, "", "jail"},
		{"cat /u/other/secret", 1, "", "jail"},
		{"ls /", 1, "", ""},
		{"echo x > /x.txt", 1, "", "jail"},
	} {
		res, err := e.Exec(context.Background(), ExecRequest{SessionKey: "s1", Script: c.script})
		if err != nil {
			t.Fatalf("%q → engine err %v", c.script, err)
		}
		if res.ExitCode != c.wantCode {
			t.Errorf("%q → exit=%d, want %d（stderr=%q）", c.script, res.ExitCode, c.wantCode, res.Stderr)
		}
		if c.wantOut != "" && !strings.Contains(res.Stdout, c.wantOut) {
			t.Errorf("%q → stdout=%q, want 含 %q", c.script, res.Stdout, c.wantOut)
		}
		if c.wantErr != "" && !strings.Contains(res.Stderr+res.Stdout, c.wantErr) {
			t.Errorf("%q → 输出不含 %q（stderr=%q）", c.script, c.wantErr, res.Stderr)
		}
	}
}

func TestEngineExecBasic(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t)
	res, err := e.Exec(context.Background(), ExecRequest{SessionKey: "s1", Script: "echo hello; echo err >&2"})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 0 || strings.TrimSpace(res.Stdout) != "hello" {
		t.Fatalf("got code=%d stdout=%q", res.ExitCode, res.Stdout)
	}
	if strings.TrimSpace(res.Stderr) != "err" {
		t.Fatalf("stderr=%q", res.Stderr)
	}
}

// UFS 直通语义：落 backing 的写跨 exec/会话可见（UFS 是持久层）；
// 内存层 per-exec 隔离（/tmp 不对其他执行可见——per-exec 会话比旧的
// per-session 更强隔离）。
func TestEngineSessionPersistsFS(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t)
	ctx := context.Background()
	if _, err := e.Exec(ctx, ExecRequest{SessionKey: "s1", Script: "echo data > f.txt"}); err != nil {
		t.Fatal(err)
	}
	res, err := e.Exec(ctx, ExecRequest{SessionKey: "s1", Script: "cat f.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(res.Stdout) != "data" {
		t.Fatalf("backing fs not shared: %q", res.Stdout)
	}
	res2, err := e.Exec(ctx, ExecRequest{SessionKey: "s2", Script: "cat f.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(res2.Stdout) != "data" {
		t.Fatalf("UFS should persist across sessions: %q", res2.Stdout)
	}
	// 内存层 per-exec 隔离：同会话键的下一次 exec 也不可见（独立内存层）。
	if _, err := e.Exec(ctx, ExecRequest{SessionKey: "s1", Script: "echo tmp > /tmp/mem-only.txt"}); err != nil {
		t.Fatal(err)
	}
	res3, err := e.Exec(ctx, ExecRequest{SessionKey: "s1", Script: "cat /tmp/mem-only.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if res3.ExitCode == 0 {
		t.Fatalf("mem layer should be per-exec isolated")
	}
}

func TestEngineRegistryHasPlatformCommands(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t)
	for _, name := range []string{"commands", "bg", "grant", "list_hosts", "send_user", "jq", "ls", "rg", "cp", "mv", "rm"} {
		if _, ok := e.Registry().Lookup(name); !ok {
			t.Fatalf("registry missing %q", name)
		}
	}
	// 内建 --help 冒烟（回归保险丝）。
	res, err := e.Exec(context.Background(), ExecRequest{SessionKey: "s1", Script: "ls --help >/dev/null && jq --help >/dev/null && commands | grep -q '^grant$'"})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("help smoke failed: code=%d stderr=%s", res.ExitCode, res.Stderr)
	}
}

func TestEngineTimeoutCap(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t)
	start := time.Now()
	res, err := e.Exec(context.Background(), ExecRequest{
		SessionKey: "s1", Script: "sleep 5", Timeout: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 124 {
		t.Fatalf("timeout exit = %d, want 124", res.ExitCode)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatalf("timeout not honored")
	}
}

// stdio 接线：Stdout/Stderr writer 全量透传（引擎只写不创建不关闭；
// 返回采集串不受接线影响）。
func TestEngineLogTeeSplit(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t)
	var outLog, errLog strings.Builder
	res, err := e.Exec(context.Background(), ExecRequest{
		SessionKey: "s1", Script: "echo teed; echo oops >&2", Stdout: &outLog, Stderr: &errLog,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(outLog.String(), "teed") || !strings.Contains(res.Stdout, "teed") {
		t.Fatalf("stdout tee broken: log=%q stdout=%q", outLog.String(), res.Stdout)
	}
	if strings.Contains(outLog.String(), "oops") {
		t.Fatalf("stderr leaked into stdout log: %q", outLog.String())
	}
	if !strings.Contains(errLog.String(), "oops") {
		t.Fatalf("stderr tee broken: %q", errLog.String())
	}
}

func TestEngineWriteAudit(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t)
	res, err := e.Exec(context.Background(), ExecRequest{SessionKey: "s1", Script: "echo x > audit-me.txt"})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, w := range res.Writes {
		if strings.HasSuffix(w, "audit-me.txt") {
			found = true
		}
	}
	if !found {
		t.Fatalf("write audit missing: %v", res.Writes)
	}
}

// chmodBacking 给 localFS 补 Chmod（对齐 host OSVFS 的执行位能力；vigo ufs
// localFS 无 Chmod，P2 回归需要带执行位语义的 backing）。
type chmodBacking struct {
	ufs.FS
	root string
}

func (b chmodBacking) Chmod(name string, mode stdfs.FileMode) error {
	return os.Chmod(filepath.Join(b.root, filepath.FromSlash(strings.TrimPrefix(name, "/"))), mode)
}

// TestHostBuiltinEchoViaStubDir 无内存层的 host 形态引擎（OS backing + 规则表
// 门）上，shell 内置名 echo 经 BuiltinCommandDir 指向真实 stub 目录后可解析
// 执行（修复前默认 /bin 在 host 上不存在 → /bin/echo ENOENT）；且布局初始化
// 写出的 stub 带执行位（0644 stub 会被 PATH/type -P 解析跳过）。
func TestHostBuiltinEchoViaStubDir(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	local, err := ufs.NewLocalFS(root)
	if err != nil {
		t.Fatal(err)
	}
	backing := chmodBacking{FS: local, root: root}
	const stubDir = "/stub/bin"
	env := map[string]string{"HOME": "/stub/home", "PATH": stubDir, "USER": "agent"}
	e, err := NewEngine(EngineConfig{
		BaseEnv: env,
		NewSessionFS: func(key string) (gbfs.FileSystem, string, error) {
			fsys, err := NewHostFS(HostFSConfig{
				Backing: backing,
				Rules:   func() vbox.FSRuleSet { return vbox.FSRuleSet{DefaultWrite: vbox.EffRW} },
			})
			return fsys, "/stub", err
		},
		LayoutEnv:         env,
		BuiltinCommandDir: stubDir,
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err := e.Exec(context.Background(), ExecRequest{SessionKey: "s1", Script: "echo hi"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(res.Stdout) != "hi" {
		t.Fatalf("echo stdout = %q (stderr %q, exit %d)", res.Stdout, res.Stderr, res.ExitCode)
	}
	info, err := backing.Stat(stubDir + "/echo")
	if err != nil {
		t.Fatalf("stub echo not pinned: %v", err)
	}
	if info.Mode()&0o111 == 0 {
		t.Fatalf("stub echo mode = %v, want executable", info.Mode())
	}
}

// 确保 io 引用保留（blockHandle 签名）。
var _ = io.Discard

// TestShellQuoteRoundtrip 用 aic ui/assets/libs/shell_quote.js 的输出形状
// （POSIX 单引号语义：'...' 内无转义；单引号用 '"'"' 闭合替换；良名直通）
// 作为测试向量，断言 vsh 解析后参数原样还原、没有额外指令被执行——
// 与 JS 侧 shell_quote.test.js 的输出形状断言互为两端（hosts-vsh-redesign
// §5「在 vsh 中验证参数原样还原且无额外执行」）。
func TestShellQuoteRoundtrip(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t)
	vectors := []struct{ raw, quoted string }{
		{"abc", "abc"},         // 良名直通
		{"", "''"},             // 空串
		{"a b", "'a b'"},       // 空白
		{"it's", `'it'"'"'s'`}, // 单引号闭合替换
		{`a\b`, `'a\b'`},       // 反斜杠字面量
		{"a$b", "'a$b'"},       // 变量符不展开
		{"a`b", "'a`b'"},       // 反引号不替换
		{"a\nb", "'a\nb'"},     // 换行
		{"*", "'*'"},           // 通配符不展开
		{"?", "'?'"},
		{"[a-z]", "'[a-z]'"},
		{"a;b", "'a;b'"}, // 分号不断句
		{"$(touch /u/u1/pwned)", "'$(touch /u/u1/pwned)'"},                   // 命令替换不执行
		{"x'; touch /u/u1/pwned2; '", `'x'"'"'; touch /u/u1/pwned2; '"'"''`}, // 引号逃逸闭合
		{"中文 值", "'中文 值'"},                                                   // Unicode
	}
	for _, v := range vectors {
		res, err := e.Exec(context.Background(), ExecRequest{SessionKey: "sq", Script: "echo " + v.quoted})
		if err != nil {
			t.Fatalf("quoted %q: %v", v.quoted, err)
		}
		got := strings.TrimSuffix(res.Stdout, "\n")
		if res.ExitCode != 0 || got != v.raw {
			t.Fatalf("quoted %q: exit=%d stdout=%q want %q (stderr %q)", v.quoted, res.ExitCode, got, v.raw, res.Stderr)
		}
	}
	// 危险向量不得产生副作用
	res, err := e.Exec(context.Background(), ExecRequest{SessionKey: "sq", Script: "ls /u/u1"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.Stdout, "pwned") {
		t.Fatalf("quoted vectors executed side effects: %q", res.Stdout)
	}
}

// TestHostBuiltinExitDispatch A 缺陷回归：无 stub 标记文件的内置名（exit）
// 在 host 形态引擎（NativeFallback 对任意良名合成）上不被重写成 stub 路径，
// 保持解释器派发。修复前 exit 被重写为 <stubDir>/exit → 127 "No such file"。
func TestHostBuiltinExitDispatch(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	local, err := ufs.NewLocalFS(root)
	if err != nil {
		t.Fatal(err)
	}
	backing := chmodBacking{FS: local, root: root}
	const stubDir = "/stub/bin"
	env := map[string]string{"HOME": "/stub/home", "PATH": stubDir, "USER": "agent"}
	native := NewNativeRegistry(NativeDeps{})
	native.SetPolicy(true, nil)
	e, err := NewEngine(EngineConfig{
		BaseEnv: env,
		NewSessionFS: func(key string) (gbfs.FileSystem, string, error) {
			fsys, err := NewHostFS(HostFSConfig{
				Backing: backing,
				Rules:   func() vbox.FSRuleSet { return vbox.FSRuleSet{DefaultWrite: vbox.EffRW} },
			})
			return fsys, "/stub", err
		},
		LayoutEnv:         env,
		BuiltinCommandDir: stubDir,
		NativeFallback:    native.OpenLookup,
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err := e.Exec(context.Background(), ExecRequest{SessionKey: "s1", Script: "exit 3"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := backing.Stat(stubDir + "/exit"); err == nil {
		t.Fatal("precondition: exit stub must not exist")
	}
	if res.ExitCode != 3 {
		t.Fatalf("exit 3 → code=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	if strings.Contains(res.Stderr, "No such file") {
		t.Fatalf("stderr leaked rewrite failure: %q", res.Stderr)
	}
}

// TestHostExplicitBinaryPath B 缺陷回归：显式程序路径的二进制文件（/bin/ls
// 形态，首行含 NUL）经 NativeFallback 按路径原生执行——旧行为把二进制当脚本
// 交默认解释器（126）。引擎路径里的 /bin/ls 只是触发解析的标记文件；原生层
// 按同一路径取真实二进制执行（参数透传，输出可验证）。
func TestHostExplicitBinaryPath(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("unix explicit path semantics")
	}
	lsPath, err := exec.LookPath("ls")
	if err != nil || !filepath.IsAbs(lsPath) {
		t.Skipf("no absolute ls on PATH: %q %v", lsPath, err)
	}
	root := t.TempDir()
	local, err := ufs.NewLocalFS(root)
	if err != nil {
		t.Fatal(err)
	}
	backing := chmodBacking{FS: local, root: root}
	const stubDir = "/stub/bin"
	env := map[string]string{"HOME": "/stub/home", "PATH": stubDir, "USER": "agent"}
	m := vbox.NewManager(0)
	m.SetNoSandbox(true)
	native := NewNativeRegistry(NativeDeps{
		Manager: m,
		Workdir: func(string) string { return "" },
	})
	native.SetPolicy(true, nil)
	e, err := NewEngine(EngineConfig{
		BaseEnv: env,
		NewSessionFS: func(key string) (gbfs.FileSystem, string, error) {
			fsys, err := NewHostFS(HostFSConfig{
				Backing: backing,
				Rules:   func() vbox.FSRuleSet { return vbox.FSRuleSet{DefaultWrite: vbox.EffRW} },
			})
			return fsys, "/stub", err
		},
		LayoutEnv:         env,
		BuiltinCommandDir: stubDir,
		NativeFallback:    native.OpenLookup,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "bin", "ls"), []byte("\x7fELF\x00\x01binary\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	markerDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(markerDir, "marker-ivec"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := e.Exec(context.Background(), ExecRequest{SessionKey: "s1", Script: lsPath + " " + markerDir})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("explicit binary path → code=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	if !strings.Contains(res.Stdout, "marker-ivec") {
		t.Fatalf("stdout = %q, want real ls output (stderr=%q)", res.Stdout, res.Stderr)
	}
}
