package host

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/veypi/vbox"
	"github.com/veypi/vsh/commands"
)

// ---- 参数解析 ----

func TestParseGitTransport(t *testing.T) {
	type want struct {
		ok     bool
		sub    string
		opts   []string
		remote string
		rest   []string
	}
	cases := []struct {
		name string
		args []string
		want want
	}{
		{"非传输子命令", []string{"status"}, want{ok: false}},
		{"非传输子命令 commit", []string{"commit", "-m", "x"}, want{ok: false}},
		{"全局选项形态", []string{"-C", "/x", "pull"}, want{ok: false}},
		{"全局内联配置形态", []string{"-c", "a=b", "push"}, want{ok: false}},
		{"空参数", nil, want{ok: false}},
		{"clone 基本", []string{"clone", "git@github.com:veypi/aic.git"},
			want{ok: true, sub: "clone", remote: "git@github.com:veypi/aic.git"}},
		{"clone 带目录与选项", []string{"clone", "--depth", "1", "-b", "main", "--single-branch", "git@github.com:x/y.git", "dst"},
			want{ok: true, sub: "clone", opts: []string{"--depth", "1", "-b", "main", "--single-branch"}, remote: "git@github.com:x/y.git", rest: []string{"dst"}}},
		{"clone 等值选项", []string{"clone", "--depth=3", "--filter=blob:none", "git@github.com:x/y.git"},
			want{ok: true, sub: "clone", opts: []string{"--depth=3", "--filter=blob:none"}, remote: "git@github.com:x/y.git"}},
		{"clone 双横线分隔", []string{"clone", "--", "git@github.com:x/y.git"},
			want{ok: true, sub: "clone", remote: "git@github.com:x/y.git"}},
		{"fetch 空", []string{"fetch"}, want{ok: true, sub: "fetch"}},
		{"fetch remote+refspec", []string{"fetch", "origin", "+refs/heads/*:refs/remotes/origin/*"},
			want{ok: true, sub: "fetch", remote: "origin", rest: []string{"+refs/heads/*:refs/remotes/origin/*"}}},
		{"pull rebase 裸形", []string{"pull", "--rebase", "origin", "main"},
			want{ok: true, sub: "pull", opts: []string{"--rebase"}, remote: "origin", rest: []string{"main"}}},
		{"pull rebase 等值", []string{"pull", "--rebase=merges"},
			want{ok: true, sub: "pull", opts: []string{"--rebase=merges"}}},
		{"push 选项组合", []string{"push", "-u", "--force-with-lease", "origin", "main"},
			want{ok: true, sub: "push", opts: []string{"-u", "--force-with-lease"}, remote: "origin", rest: []string{"main"}}},
		{"push force-with-lease 等值", []string{"push", "--force-with-lease=main:abc", "origin"},
			want{ok: true, sub: "push", opts: []string{"--force-with-lease=main:abc"}, remote: "origin"}},
		{"push 删除引用", []string{"push", "--delete", "origin", "old"},
			want{ok: true, sub: "push", opts: []string{"--delete"}, remote: "origin", rest: []string{"old"}}},
		{"ls-remote 默认", []string{"ls-remote"},
			want{ok: true, sub: "ls-remote"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			call, ok, err := parseGitTransport(tc.args)
			if ok != tc.want.ok {
				t.Fatalf("ok=%v err=%v", ok, err)
			}
			if !ok {
				return
			}
			if err != nil {
				t.Fatalf("err=%v", err)
			}
			if call.sub != tc.want.sub || call.remote != tc.want.remote {
				t.Fatalf("call=%+v", call)
			}
			if strings.Join(call.opts, "\x00") != strings.Join(tc.want.opts, "\x00") ||
				strings.Join(call.rest, "\x00") != strings.Join(tc.want.rest, "\x00") {
				t.Fatalf("call=%+v want=%+v", call, tc.want)
			}
		})
	}
}

func TestParseGitTransportRejections(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"clone 内联配置", []string{"clone", "-c", "core.sshCommand=x", "git@h:r"}, "core.sshCommand"},
		{"clone upload-pack", []string{"clone", "--upload-pack", "/tmp/x", "git@h:r"}, "upload-pack"},
		{"clone 模板", []string{"clone", "--template", "/tmp/t", "git@h:r"}, "模板"},
		{"clone 子模块", []string{"clone", "--recurse-submodules", "git@h:r"}, "子模块"},
		{"clone 未知选项", []string{"clone", "--ipv99", "git@h:r"}, "白名单"},
		{"clone 缺 remote", []string{"clone", "--depth", "1"}, "缺少 remote"},
		{"clone 选项缺值", []string{"clone", "--depth"}, "缺少参数值"},
		{"clone 参数过多", []string{"clone", "a", "b", "c"}, "过多"},
		{"fetch --all", []string{"fetch", "--all"}, "多 remote"},
		{"pull 编辑器", []string{"pull", "--edit"}, "编辑器"},
		{"push --force", []string{"push", "--force", "origin", "main"}, "破坏性"},
		{"push -f", []string{"push", "-f"}, "破坏性"},
		{"push mirror", []string{"push", "--mirror"}, "破坏性"},
		{"push +refspec", []string{"push", "origin", "+main:main"}, "--force"},
		{"push 星号 refspec", []string{"push", "origin", "*"}, "形态不允许"},
		{"push receive-pack", []string{"push", "--receive-pack", "/tmp/x"}, "receive-pack"},
		{"refspec 双点", []string{"fetch", "origin", "a..b"}, "形态不允许"},
		{"布尔选项赋值", []string{"fetch", "--prune=yes"}, "不接受赋值"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			call, ok, err := parseGitTransport(tc.args)
			if !ok {
				t.Fatalf("ok=false（应识别为受管对象并拒绝）")
			}
			if call != nil {
				t.Fatalf("拒绝时不应产出 call: %+v", call)
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err=%v want 含 %q", err, tc.want)
			}
		})
	}
}

// ---- remote 分类与目标 ----

func TestClassifyGitRemote(t *testing.T) {
	cases := []struct {
		ref  string
		want gitRemoteKind
	}{
		{"git@github.com:veypi/aic.git", gitRemoteSSH},
		{"github.com:veypi/aic.git", gitRemoteSSH},
		{"fixture:/srv/repos/a.git", gitRemoteSSH},
		{"ssh://git@github.com/veypi/aic.git", gitRemoteSSH},
		{"ssh://git@github.com:2222/veypi/aic.git", gitRemoteSSH},
		{"ssh://github.com/x.git", gitRemoteSSH},
		{"https://github.com/veypi/aic.git", gitRemoteOther},
		{"http://x/y.git", gitRemoteOther},
		{"git://x/y.git", gitRemoteOther},
		{"file:///srv/a.git", gitRemoteOther},
		{"/srv/repos/a.git", gitRemoteLocal},
		{"./rel/path", gitRemoteLocal},
		{"../rel/path", gitRemoteLocal},
		{"~/rel/path", gitRemoteLocal},
		{"rel/path", gitRemoteLocal},
		{"origin", gitRemoteLocal},
	}
	for _, tc := range cases {
		if got := classifyGitRemote(tc.ref); got != tc.want {
			t.Errorf("%q: got %d want %d", tc.ref, got, tc.want)
		}
	}
}

func TestGitSSHTarget(t *testing.T) {
	cases := []struct {
		ref, want string
	}{
		{"git@github.com:veypi/aic.git", "git@github.com"},
		{"github.com:veypi/aic.git", "github.com"},
		{"fixture:/srv/a.git", "fixture"},
		{"ssh://git@github.com/veypi/aic.git", "git@github.com"},
		{"ssh://git@github.com:2222/veypi/aic.git", "git@github.com:2222"},
		{"ssh://github.com/x.git", "github.com"},
		{"ssh://git:secret@github.com/x.git", "git@github.com"}, // ssh 不用口令，丢弃
	}
	for _, tc := range cases {
		raw, kind := gitSSHTarget(tc.ref)
		if kind != gitRemoteSSH || raw != tc.want {
			t.Errorf("%q: raw=%q kind=%d want %q", tc.ref, raw, kind, tc.want)
		}
	}
	for _, ref := range []string{"https://github.com/x/y.git", "/srv/a.git", "./rel"} {
		if _, kind := gitSSHTarget(ref); kind == gitRemoteSSH {
			t.Errorf("%q: 不应判为 ssh", ref)
		}
	}
}

// ---- .git/config 与 HEAD ----

func writeGitRepo(t *testing.T, config string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".git", "config"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestGitRemoteForCall(t *testing.T) {
	repo := writeGitRepo(t, `[core]
	repositoryformatversion = 0
[remote "origin"]
	url = git@github.com:veypi/aic.git
	fetch = +refs/heads/*:refs/remotes/origin/*
[remote "UPstream"]
	url = ssh://git@example.com:2222/x/y.git
[branch "main"]
	remote = UPstream
	merge = refs/heads/main
`)
	// 显式 remote 名
	target, err := gitRemoteForCall(repo, &gitTransportCall{sub: "fetch", remote: "origin"})
	if err != nil || target.url != "git@github.com:veypi/aic.git" || target.passArg != "origin" {
		t.Fatalf("origin: %+v err=%v", target, err)
	}
	// 缺省 → 跟随分支配置（UPstream，大小写敏感）
	target, err = gitRemoteForCall(repo, &gitTransportCall{sub: "pull"})
	if err != nil || target.url != "ssh://git@example.com:2222/x/y.git" || target.passArg != "UPstream" {
		t.Fatalf("branch remote: %+v err=%v", target, err)
	}
	// 显式 URL 直通
	target, err = gitRemoteForCall(repo, &gitTransportCall{sub: "fetch", remote: "git@h:r/x.git"})
	if err != nil || target.url != "git@h:r/x.git" || target.passArg != "git@h:r/x.git" {
		t.Fatalf("url passthrough: %+v err=%v", target, err)
	}
	// clone = URL 本身
	target, err = gitRemoteForCall(repo, &gitTransportCall{sub: "clone", remote: "git@h:r/x.git"})
	if err != nil || target.url != "git@h:r/x.git" {
		t.Fatalf("clone: %+v err=%v", target, err)
	}
	// 未知名
	if _, err = gitRemoteForCall(repo, &gitTransportCall{sub: "fetch", remote: "nope"}); err == nil ||
		!strings.Contains(err.Error(), "nope") {
		t.Fatalf("unknown remote err=%v", err)
	}
	// 非仓库
	if _, err = gitRemoteForCall(t.TempDir(), &gitTransportCall{sub: "fetch"}); err == nil ||
		!strings.Contains(err.Error(), "不是 git 仓库") {
		t.Fatalf("non-repo err=%v", err)
	}
	// 分支无 remote 配置 → 回退 origin
	repo2 := writeGitRepo(t, "[remote \"origin\"]\n\turl = git@h:o/x.git\n")
	target, err = gitRemoteForCall(repo2, &gitTransportCall{sub: "push"})
	if err != nil || target.passArg != "origin" {
		t.Fatalf("fallback origin: %+v err=%v", target, err)
	}
}

func TestGitConfigValue(t *testing.T) {
	data := []byte(`# comment
[remote "origin"]
	url = "git@quoted host:x/y.git"
	URL = second@entry:x.git
[branch "main"]
	remote = origin ; trailing comment
[remote.oldstyle]
	url = old@x:y.git
`)
	// 引号值 + key 大小写不敏感（首个匹配生效）
	if v, ok := gitConfigValue(data, "remote", "origin", "url"); !ok || v != "git@quoted host:x/y.git" {
		t.Fatalf("quoted: %q %v", v, ok)
	}
	if v, ok := gitConfigValue(data, "BRANCH", "main", "REMOTE"); !ok || v != "origin ; trailing comment" {
		t.Fatalf("branch: %q %v", v, ok)
	}
	if v, ok := gitConfigValue(data, "remote", "oldstyle", "url"); !ok || v != "old@x:y.git" {
		t.Fatalf("oldstyle: %q %v", v, ok)
	}
	if _, ok := gitConfigValue(data, "remote", "missing", "url"); ok {
		t.Fatal("missing hit")
	}
}

// ---- wrapper 脚本 ----

func TestGitSSHWrapperScript(t *testing.T) {
	argv := []string{"/usr/bin/ssh", "-F", "none", "-o", "UserKnownHostsFile=\"/tmp/has space/known_hosts\"",
		"-p", "22", "-l", "git", "--", "github.com"}
	s := gitSSHWrapperScript(argv)
	for _, want := range []string{
		"#!/bin/sh\n", "-o|-p) shift 2", "shift; fi",
		"exec /usr/bin/ssh -F none",
		`-o 'UserKnownHostsFile="/tmp/has space/known_hosts"'`, // 含空格与引号的值整体单引号化
		"-- github.com", `"$@"`,
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("wrapper 缺 %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "has space/known_hosts\" -p") {
		t.Fatalf("引号值被拆断:\n%s", s)
	}
}

func TestShellQuoteArg(t *testing.T) {
	if got := shellQuoteArg("github.com"); got != "github.com" {
		t.Fatalf("plain: %q", got)
	}
	if got := shellQuoteArg("a b"); got != `'a b'` {
		t.Fatalf("space: %q", got)
	}
	if got := shellQuoteArg(`it's`); got != `'it'\''s'` {
		t.Fatalf("quote: %q", got)
	}
	if got := shellQuoteArg(""); got != `''` {
		t.Fatalf("empty: %q", got)
	}
}

// ---- managedGit 分流与受管执行 ----

func gitTestClient(t *testing.T) (*Client, string) {
	t.Helper()
	c, _ := testClient(t)
	return c, c.options().WorkDir
}

func TestManagedGitFallthrough(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("managed git 未接入 windows")
	}
	c, wd := gitTestClient(t)
	inv := &commands.Invocation{Cwd: wd, Stdout: &strings.Builder{}, Stderr: &strings.Builder{}}
	for _, args := range [][]string{
		{"status"}, {"log", "--oneline"}, {"commit", "-m", "x"}, {"diff"},
		{"-C", "/x", "pull"},         // 全局选项形态
		{"clone", "https://h/x.git"}, // https → 原生（MVP）
		{"clone", "/srv/local.git"},  // 本地路径 → 原生
		{"clone", "./rel.git"},       // 相对路径 → 原生
		{"submodule", "update"},      // 非白名单子命令
		{"remote", "update"},         // 非白名单子命令
	} {
		handled, err := c.managedGit(context.Background(), &commands.Invocation{
			Args: args, Cwd: inv.Cwd, Stdout: inv.Stdout, Stderr: inv.Stderr})
		if handled || err != nil {
			t.Errorf("%v: handled=%v err=%v（应交还原生）", args, handled, err)
		}
	}
}

func TestManagedGitDeniedWithoutGrant(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("managed git 未接入 windows")
	}
	c, wd := gitTestClient(t)
	var out, errOut strings.Builder
	inv := &commands.Invocation{
		Args: []string{"clone", "git@github.com:veypi/aic.git"},
		Cwd:  wd, Stdout: &out, Stderr: &errOut,
	}
	handled, err := c.managedGit(context.Background(), inv)
	if !handled {
		t.Fatal("ssh 传输未接管")
	}
	if code, _ := commands.ExitCode(err); code != 126 {
		t.Fatalf("code=%d err=%v", code, err)
	}
	if !strings.Contains(errOut.String(), "grant ssh github.com:22") {
		t.Fatalf("拒绝文案缺 grant 指引: %q", errOut.String())
	}
}

// TestManagedGitManagedRun 授权后：受管执行用系统 git、钉住 hooksPath 与
// GIT_SSH wrapper；wrapper 内嵌授权目标；结束后临时目录清理。
func TestManagedGitManagedRun(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("managed git 未接入 windows")
	}
	c, wd := gitTestClient(t)
	if _, err := c.grantTarget("", "ssh", "github.com:22", false); err != nil {
		t.Fatal(err)
	}
	var captured vbox.StartOptions
	var wrapperPath string
	c.gitRun = func(_ context.Context, opts vbox.StartOptions, _ io.Reader, _, _ io.Writer) (int, error) {
		captured = opts
		for _, e := range opts.Env {
			if strings.HasPrefix(e, "GIT_SSH=") {
				wrapperPath = strings.TrimPrefix(e, "GIT_SSH=")
			}
		}
		if wrapperPath == "" {
			t.Error("env 缺 GIT_SSH")
			return 1, nil
		}
		data, err := os.ReadFile(wrapperPath)
		if err != nil {
			t.Errorf("wrapper 不可读: %v", err)
			return 1, nil
		}
		s := string(data)
		for _, want := range []string{"-- github.com", "-l git", "BatchMode=yes", `"$@"`} {
			if !strings.Contains(s, want) {
				t.Errorf("wrapper 缺 %q:\n%s", want, s)
			}
		}
		return 0, nil
	}
	var out, errOut strings.Builder
	inv := &commands.Invocation{
		Args: []string{"clone", "--depth", "1", "git@github.com:veypi/aic.git", "dst"},
		Cwd:  wd, Stdout: &out, Stderr: &errOut,
	}
	handled, err := c.managedGit(context.Background(), inv)
	if !handled || err != nil {
		t.Fatalf("handled=%v err=%v stderr=%q", handled, err, errOut.String())
	}
	gitBin, _ := trustedGitBinary()
	wantArgv := []string{gitBin, "-c", "core.hooksPath=/dev/null", "clone", "--depth", "1", "git@github.com:veypi/aic.git", "dst"}
	if strings.Join(captured.Exec, "\x00") != strings.Join(wantArgv, "\x00") {
		t.Fatalf("argv=%v want %v", captured.Exec, wantArgv)
	}
	if !captured.NoSandbox || !captured.ReplaceEnv {
		t.Fatalf("受管执行必须 NoSandbox+ReplaceEnv: %+v", captured)
	}
	if captured.Workdir != wd {
		t.Fatalf("workdir=%q want %q", captured.Workdir, wd)
	}
	envJoined := strings.Join(captured.Env, "\n")
	for _, want := range []string{"GIT_SSH_VARIANT=ssh", "GIT_TERMINAL_PROMPT=0", "HOME="} {
		if !strings.Contains(envJoined, want) {
			t.Fatalf("env 缺 %q:\n%s", want, envJoined)
		}
	}
	if wrapperPath == "" {
		t.Fatal("未捕获 wrapper 路径")
	}
	if _, err := os.Stat(wrapperPath); !os.IsNotExist(err) {
		t.Fatalf("wrapper 未清理: %v", err)
	}
}

// TestManagedGitExitCode passthrough：远端拒绝等 git 自身非零退出码原样上抛。
func TestManagedGitExitCode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("managed git 未接入 windows")
	}
	c, wd := gitTestClient(t)
	if _, err := c.grantTarget("", "ssh", "github.com:22", false); err != nil {
		t.Fatal(err)
	}
	c.gitRun = func(context.Context, vbox.StartOptions, io.Reader, io.Writer, io.Writer) (int, error) {
		return 128, nil
	}
	inv := &commands.Invocation{
		Args: []string{"clone", "git@github.com:x/y.git"},
		Cwd:  wd, Stdout: &strings.Builder{}, Stderr: &strings.Builder{},
	}
	handled, err := c.managedGit(context.Background(), inv)
	if code, _ := commands.ExitCode(err); !handled || code != 128 {
		t.Fatalf("handled=%v code=%d err=%v", handled, code, err)
	}
}
