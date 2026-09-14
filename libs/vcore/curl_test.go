package vcore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/veypi/aic-pod/libs/proto"
	"github.com/veypi/vigo/contrib/ufs"
)

// TestDropUnknownFlags 锁定 curl 宽松 flag 语义：未知 flag 剥离（不报错），
// 已知 flag 与纯已知 bool 组合保留，位置参数不受影响。
func TestDropUnknownFlags(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{"plain", []string{"https://example.com"}, []string{"https://example.com"}},
		{"unknown bool combo", []string{"-sL", "https://example.com"}, []string{"https://example.com"}},
		{"unknown single", []string{"-k", "https://example.com"}, []string{"https://example.com"}},
		{"known -L kept", []string{"-L", "https://example.com"}, []string{"-L", "https://example.com"}},
		{"known -o kept with value", []string{"-o", "/tmp/x", "https://example.com"}, []string{"-o", "/tmp/x", "https://example.com"}},
		{"known value kept", []string{"--max-size", "5", "https://example.com"}, []string{"--max-size", "5", "https://example.com"}},
		{"known bool combo", []string{"-LL", "https://example.com"}, []string{"-LL", "https://example.com"}},
		{"mixed unknown+known", []string{"-sL", "-o", "/tmp/x", "https://example.com"}, []string{"-o", "/tmp/x", "https://example.com"}},
		{"long unknown", []string{"--compressed", "https://example.com"}, []string{"https://example.com"}},
		{"bare dash kept", []string{"-", "https://example.com"}, []string{"-", "https://example.com"}},
	}
	for _, c := range cases {
		got := dropUnknownFlags(c.in)
		if strings.Join(got, " ") != strings.Join(c.want, " ") {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

// TestCurlLenientFlags 集成验证：未声明 flag（-sL/-k 等真实 curl 习惯参数）
// 静默剥离后正常执行，不再报 restricted 错误。
func TestCurlLenientFlags(t *testing.T) {
	env := &Env{
		VFS:     NewMemVFS(),
		Workdir: "/sessions/s1",
		Fetcher: FetchFunc(func(ctx context.Context, req HTTPReq) (io.ReadCloser, int64, error) {
			return io.NopCloser(strings.NewReader("hello")), 5, nil
		}),
		Tasks:        testTaskRunner{},
		TaskID:       "t1",
		Roots:        []string{"/"},
		ProtectRoots: []string{"/"},
	}
	cases := [][]string{
		{"-sL", "https://example.com"},
		{"-k", "-s", "https://example.com"},
		{"--compressed", "https://example.com"},
	}
	for _, argv := range cases {
		res, err := Run(context.Background(), env, "curl", argv)
		if err != nil {
			t.Errorf("curl %v: unexpected error: %v", argv, err)
			continue
		}
		if res == nil || res.Content != "hello" {
			t.Errorf("curl %v: content = %+v, want hello", argv, res)
		}
	}
	// -o 形态：未知 flag 剥离后正常落盘（content 为下载摘要）
	res, err := Run(context.Background(), env, "curl", []string{"-sL", "-o", "/sessions/s1/a.txt", "https://example.com"})
	if err != nil {
		t.Fatalf("curl -sL -o: unexpected error: %v", err)
	}
	if !strings.Contains(res.Content, "downloaded https://example.com to /sessions/s1/a.txt") {
		t.Errorf("curl -sL -o: content = %q", res.Content)
	}
	if data, err := env.VFS.ReadFile("/sessions/s1/a.txt"); err != nil || string(data) != "hello" {
		t.Errorf("curl -sL -o: file = %q, err=%v; want hello", data, err)
	}
	// 未知 flag 剥离后位置参数多余（如未知带值 flag 的值）仍由 maxPos 兜底
	if _, err := Run(context.Background(), env, "curl", []string{"--foo", "bar", "https://example.com"}); err == nil {
		t.Error("unknown value flag leftovers should hit maxPos check")
	}
}

// TestCurlDevNullAndWriteOut 锁定用户场景：-o /dev/null 丢弃响应不建文件、
// -w（已知带值未实现 flag）连值剥离不残留成位置参数。
func TestCurlDevNullAndWriteOut(t *testing.T) {
	var got HTTPReq
	env := &Env{
		VFS:     NewMemVFS(),
		Workdir: "/sessions/s1",
		Fetcher: FetchFunc(func(ctx context.Context, req HTTPReq) (io.ReadCloser, int64, error) {
			got = req
			return io.NopCloser(strings.NewReader("hello")), 5, nil
		}),
		Tasks:  testTaskRunner{},
		TaskID: "t1",
		Roots:  []string{"/"},
	}

	// 用户原始场景：-s -o /dev/null -w "format" url → 不再报 unexpected argument
	res, err := Run(context.Background(), env, "curl", []string{
		"-s", "-o", "/dev/null", "-w", "HTTP状态码: %{http_code}\n耗时: %{time_total}s\n", "https://www.example.com",
	})
	if err != nil {
		t.Fatalf("user scenario: %v", err)
	}
	if !strings.Contains(res.Content, "discarded response from https://www.example.com (5 bytes)") {
		t.Errorf("content = %q", res.Content)
	}
	// 未在会话空间创建 /dev/null 文件
	if _, err := env.VFS.Stat("/dev/null"); err == nil {
		t.Error("-o /dev/null should not create a file in the space")
	}
	// -o /dev/null 不触发 fs 门控（无需写文件）
	envNoFS := &Env{
		VFS:     nil,
		Workdir: "/sessions/s1",
		Fetcher: FetchFunc(func(ctx context.Context, req HTTPReq) (io.ReadCloser, int64, error) {
			return io.NopCloser(strings.NewReader("hello")), 5, nil
		}),
		Tasks:  testTaskRunner{},
		TaskID: "t1",
	}
	if _, err := Run(context.Background(), envNoFS, "curl", []string{"-o", "/dev/null", "https://example.com"}); err != nil {
		t.Errorf("-o /dev/null without fs: %v", err)
	}
	_ = got
}

// TestCurlMethods 锁定 HTTP 方法/请求体/请求头：-d 隐式 POST、-X 显式方法、
// -H 头解析、非法方法与无体方法带体报错。
func TestCurlMethods(t *testing.T) {
	var got HTTPReq
	capture := func(ctx context.Context, req HTTPReq) (io.ReadCloser, int64, error) {
		got = req
		return io.NopCloser(strings.NewReader("ok")), 2, nil
	}
	env := &Env{
		VFS:     NewMemVFS(),
		Workdir: "/sessions/s1",
		Fetcher: FetchFunc(capture),
		Tasks:   testTaskRunner{},
		TaskID:  "t1",
		Roots:   []string{"/"},
	}

	// -d 隐式 POST + Content-Type 默认头
	if _, err := Run(context.Background(), env, "curl", []string{"-d", "a=1&b=2", "https://example.com/api"}); err != nil {
		t.Fatalf("-d: %v", err)
	}
	if got.Method != "POST" || string(got.Body) != "a=1&b=2" {
		t.Errorf("-d implicit POST: method=%s body=%q", got.Method, got.Body)
	}

	// -X PUT + -d + -H（重复头后者覆盖）
	if _, err := Run(context.Background(), env, "curl", []string{
		"-X", "PUT", "-d", `{"x":1}`, "-H", "Content-Type: application/json", "-H", "X-Token: abc",
		"-H", "X-Token: def", "https://example.com/api",
	}); err != nil {
		t.Fatalf("-X PUT: %v", err)
	}
	if got.Method != "PUT" || string(got.Body) != `{"x":1}` {
		t.Errorf("-X PUT: method=%s body=%q", got.Method, got.Body)
	}
	if got.Headers["Content-Type"] != "application/json" || got.Headers["X-Token"] != "def" {
		t.Errorf("-H: headers = %v", got.Headers)
	}

	// 显式 -X POST（无 -d）
	if _, err := Run(context.Background(), env, "curl", []string{"-X", "POST", "https://example.com/api"}); err != nil {
		t.Fatalf("-X POST: %v", err)
	}
	if got.Method != "POST" || len(got.Body) != 0 {
		t.Errorf("-X POST: method=%s body=%q", got.Method, got.Body)
	}

	// 非法方法报错
	if _, err := Run(context.Background(), env, "curl", []string{"-X", "OPTIONS", "https://example.com"}); err == nil || !strings.Contains(err.Error(), "method \"OPTIONS\" not supported") {
		t.Errorf("-X OPTIONS: err = %v", err)
	}
	// GET 带体报错
	if _, err := Run(context.Background(), env, "curl", []string{"-d", "x=1", "-X", "GET", "https://example.com"}); err == nil || !strings.Contains(err.Error(), "not allowed with method GET") {
		t.Errorf("GET -d: err = %v", err)
	}
	// 非法头格式报错
	if _, err := Run(context.Background(), env, "curl", []string{"-H", "NoColon", "https://example.com"}); err == nil || !strings.Contains(err.Error(), "invalid header") {
		t.Errorf("bad header: err = %v", err)
	}
}

// TestCurlUserAgentAndMaxTime 锁定 -A 注入 User-Agent（覆盖 -H 同名）与
// --max-time 请求超时（context 生效 + 参数校验）。
func TestCurlUserAgentAndMaxTime(t *testing.T) {
	var got HTTPReq
	env := &Env{
		VFS:     NewMemVFS(),
		Workdir: "/sessions/s1",
		Fetcher: FetchFunc(func(ctx context.Context, req HTTPReq) (io.ReadCloser, int64, error) {
			got = req
			// 模拟慢响应：--max-time 后 ctx 应被取消
			select {
			case <-ctx.Done():
				return nil, 0, ctx.Err()
			case <-time.After(2 * time.Second):
			}
			return io.NopCloser(strings.NewReader("ok")), 2, nil
		}),
		Tasks:  testTaskRunner{},
		TaskID: "t1",
		Roots:  []string{"/"},
	}

	// -A 注入 User-Agent（-H 同名被覆盖）
	if _, err := Run(context.Background(), env, "curl", []string{
		"-A", "aic-agent/1.0", "-H", "User-Agent: other", "https://example.com",
	}); err != nil {
		t.Fatalf("-A: %v", err)
	}
	if got.Headers["User-Agent"] != "aic-agent/1.0" {
		t.Errorf("-A: User-Agent = %q", got.Headers["User-Agent"])
	}

	// --max-time 1s：慢响应应在 1s 内被 ctx 超时中止（而非 2s 后返回）
	start := time.Now()
	_, err := Run(context.Background(), env, "curl", []string{"--max-time", "1", "https://example.com"})
	if err == nil {
		t.Fatal("--max-time: expected timeout error")
	}
	if elapsed := time.Since(start); elapsed > 1500*time.Millisecond {
		t.Errorf("--max-time: took %v, want ~1s timeout", elapsed)
	}

	// 参数校验：0/非法/超限
	for _, bad := range []string{"0", "abc", "601"} {
		if _, err := Run(context.Background(), env, "curl", []string{"--max-time", bad, "https://example.com"}); err == nil {
			t.Errorf("--max-time %s: expected error", bad)
		}
	}
}

// strategyVFS 模拟 cloud GatedFS 的写分级拒：写方法返回同一策略错误实例。
type strategyVFS struct {
	ufs.FS
	err error
}

func (v strategyVFS) MkdirAll(p string, perm os.FileMode) error { return v.err }
func (v strategyVFS) Create(name string) (ufs.File, error)      { return nil, v.err }

// TestExecVFSErrStrategyKept 锁定 execVFSErr/fsVFSErr：策略类错误（Approval/
// Denied）保型上抛，其余归一为指令错误——审批/拒绝语义不能被拍平成执行错误。
func TestExecVFSErrStrategyKept(t *testing.T) {
	ae := &proto.ApprovalError{Reason: "fs: write requires fs level 3 for /u/u1/f.bin"}
	got := execVFSErr("curl", ae, "%s", ae)
	var gotA *proto.ApprovalError
	if !errors.As(got, &gotA) || gotA != ae {
		t.Errorf("Approval 应保型: got %v", got)
	}
	// 包装链中的策略错误同样保型（提取根错误）
	wrapped := fmt.Errorf("ctx: %w", ae)
	got = execVFSErr("curl", wrapped, "%s", wrapped)
	if !errors.As(got, &gotA) || gotA != ae {
		t.Errorf("wrapped Approval 应保型: got %v", got)
	}
	de := &proto.DeniedError{Reason: "deny"}
	got = fsVFSErr("rm", de, "%s", de)
	var gotD *proto.DeniedError
	if !errors.As(got, &gotD) || gotD != de {
		t.Errorf("Denied 应保型: got %v", got)
	}
	// 非策略错误：照常归一为 ExecError
	ioErr := fmt.Errorf("disk broke")
	got = execVFSErr("curl", ioErr, "cannot create %s: %s", "/x", ioErr)
	var execE *proto.ExecError
	if !errors.As(got, &execE) || !strings.Contains(execE.Error(), "cannot create /x: disk broke") {
		t.Errorf("非策略错误应归一为 ExecError: %v", got)
	}
}

// TestCurlOutputStrategyErrorKept 集成：curl -o 的 VFS 写被策略拒绝（GatedFS
// 写分级）时，Run 返回保型的 *proto.ApprovalError（而非拍平的 ExecError）——
// cloud 内联审批流依赖类型判定（aic procs 转 NeedApprovalError → waiting）。
func TestCurlOutputStrategyErrorKept(t *testing.T) {
	ae := &proto.ApprovalError{Reason: "fs: write requires fs level 3 for /sessions/s1/a.txt"}
	env := &Env{
		VFS:     strategyVFS{FS: NewMemVFS(), err: ae},
		Workdir: "/sessions/s1",
		Fetcher: FetchFunc(func(ctx context.Context, req HTTPReq) (io.ReadCloser, int64, error) {
			return io.NopCloser(strings.NewReader("hello")), 5, nil
		}),
		Tasks:  testTaskRunner{},
		TaskID: "t1",
		Roots:  []string{"/"},
	}
	_, err := Run(context.Background(), env, "curl", []string{"-o", "/sessions/s1/a.txt", "https://example.com"})
	var gotA *proto.ApprovalError
	if !errors.As(err, &gotA) || gotA != ae {
		t.Fatalf("curl -o 策略错误应保型上抛, got %v", err)
	}
}
