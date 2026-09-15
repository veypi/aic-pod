package vcore

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// denyGitPolicy 是测试用 PathPolicy：deny 路径读级 0（不可审批），其余 1/3。
type denyGitPolicy struct{ deny string }

func (p denyGitPolicy) Decide(path string) (int, int) {
	if path == p.deny {
		return 0, 0
	}
	return 1, 3
}

// ls 的 .git 基本探测：未 deny 时上报 is_repo/branch；.git 被 deny 时跳过探测
// （不泄露分支名，也不报错）。
func TestLsGitRepoPolicyDeny(t *testing.T) {
	mt := time.Unix(0, 0)
	build := func() *MemVFS {
		v := NewMemVFS()
		v.SetDir("/repo", mt)
		v.SetFile("/repo/.git/HEAD", []byte("ref: refs/heads/main\n"), mt)
		v.SetFile("/repo/a.txt", []byte("a"), mt)
		return v
	}

	env := &Env{VFS: build(), Workdir: "/", Policy: denyGitPolicy{deny: "/other"}, Granted: 9}
	res, err := RunFS(context.Background(), env, []byte(`{"action":"ls","path":"/repo"}`))
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		IsRepo bool   `json:"is_repo"`
		Branch string `json:"branch"`
	}
	if err := json.Unmarshal([]byte(res.Content), &out); err != nil {
		t.Fatal(err)
	}
	if !out.IsRepo || out.Branch != "main" {
		t.Fatalf("is_repo=%v branch=%q, want true/main", out.IsRepo, out.Branch)
	}

	env = &Env{VFS: build(), Workdir: "/", Policy: denyGitPolicy{deny: "/repo/.git"}, Granted: 9}
	res, err = RunFS(context.Background(), env, []byte(`{"action":"ls","path":"/repo"}`))
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"cwd":"/repo","dir":true,"items":[{"name":"a.txt","dir":false,"size":1,"mod_time":0}],"truncated":false}`
	if res.Content != want {
		t.Fatalf("content = %s, want %s", res.Content, want)
	}
}

// 虚拟根（windows 盘符挂载列表）：ls 根层条目不递归（省略 items）、不做 .git 探测；
// rg path="/" 拒绝（遍历全部盘符无界，须指定盘符路径）。
func TestVirtualRoot(t *testing.T) {
	mt := time.Unix(0, 0)
	build := func() *MemVFS {
		v := NewMemVFS()
		v.SetDir("/C:", mt)
		v.SetDir("/C:/repo", mt)
		v.SetFile("/C:/repo/a.txt", []byte("hello"), mt)
		v.SetFile("/C:/.git/HEAD", []byte("ref: refs/heads/main\n"), mt)
		return v
	}

	// ls "/" depth 3：盘符条目仅显示，不递归（items 省略）、不探测 .git
	env := &Env{VFS: build(), Workdir: "/", VirtualRoot: true, Granted: 9}
	res, err := RunFS(context.Background(), env, []byte(`{"action":"ls","path":"/","depth":3}`))
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Items []struct {
			Name   string     `json:"name"`
			Dir    bool       `json:"dir"`
			IsRepo bool       `json:"is_repo"`
			Items  *[]lsEntry `json:"items"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(res.Content), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Items) != 1 || out.Items[0].Name != "C:" || !out.Items[0].Dir {
		t.Fatalf("items = %+v", out.Items)
	}
	if out.Items[0].Items != nil || out.Items[0].IsRepo {
		t.Fatalf("virtual root entry must not descend or probe .git: %+v", out.Items[0])
	}

	// 非虚拟根行为不变：depth 3 递归进子目录
	env2 := &Env{VFS: build(), Workdir: "/", Granted: 9}
	res, err = RunFS(context.Background(), env2, []byte(`{"action":"ls","path":"/","depth":3}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(res.Content), &out); err != nil {
		t.Fatal(err)
	}
	if out.Items[0].Items == nil {
		t.Fatal("non-virtual root should descend")
	}

	// rg path="/"：内容搜索与文件列举两模式均拒绝
	for _, params := range []string{
		`{"action":"rg","path":"/","pattern":"hello"}`,
		`{"action":"rg","path":"/"}`,
	} {
		if _, err := RunFS(context.Background(), env, []byte(params)); err == nil ||
			!strings.Contains(err.Error(), "virtual drive list") {
			t.Fatalf("rg / err = %v, want virtual drive list rejection", err)
		}
	}
	// 盘符路径正常工作
	res, err = RunFS(context.Background(), env, []byte(`{"action":"rg","path":"/C:","pattern":"hello"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Content, "a.txt") {
		t.Fatalf("rg /C: content = %s", res.Content)
	}
}
