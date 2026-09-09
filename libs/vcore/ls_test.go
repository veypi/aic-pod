package vcore

import (
	"context"
	"encoding/json"
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
