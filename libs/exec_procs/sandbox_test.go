package exec_procs

import (
	"runtime"
	"strings"
	"testing"

	"github.com/veypi/aic-pod/libs/proto"
)

// Confine 无豁免分支：9（审批通过）同样进沙箱——审批只是等级语义，
// 免沙箱不经 Confine 表达（StartOptions.NoSandbox 才是唯一通道）。
func TestConfineApprovedStillConfined(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("windows uses token injection, covered by TestWindowsSandbox*")
	}
	argv := []string{"bash", "-c", "echo hi"}
	got, err := Confine(proto.LevelApproved, "/ws", argv)
	if err != nil {
		t.Logf("Confine(9) unavailable on this host (fail-closed): %v", err)
		return
	}
	if len(got) == 0 || got[0] == argv[0] {
		t.Fatalf("Confine(9) = %v, want wrapped argv (approval does not exempt sandbox)", got)
	}
}

// Confine 的 confined 分支：本机有后端（darwin=seatbelt / linux=bwrap）时
// 返回包装 argv 而非原样；无后端平台（windows/其他）返回 fail-closed 错误。
// level 0（未设置/异常）与 1 同按 read-only 包装（fail-closed 兑底）。
// windows 是 argv 原样 + token 注入语义，由 sandbox_windows_test.go 覆盖。
func TestConfineConfined(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("windows uses token injection, covered by TestWindowsSandbox*")
	}
	argv := []string{"bash", "-c", "echo hi"}
	for _, level := range []int{0, proto.LevelRead} {
		got, err := Confine(level, "/ws", argv)
		if err != nil {
			t.Logf("Confine(%d) unavailable on this host (fail-closed): %v", level, err)
			continue
		}
		if len(got) == 0 || got[0] == argv[0] {
			t.Fatalf("Confine(%d) = %v, want wrapped argv", level, got)
		}
	}
}

// bwrap 包装：read-only 无任何可写挂载；workspace-write 有 tmpfs /tmp +
// 工作区 bind + 缓存目录 bind + 敏感子路径只读覆盖；命令在 -- 之后原样。
func TestBwrapArgs(t *testing.T) {
	argv := []string{"bash", "-c", "echo hi"}

	ro := bwrapArgs(proto.LevelRead, "/ws", nil, nil, argv)
	want := []string{"bwrap", "--ro-bind", "/", "/", "--dev", "/dev", "--proc", "/proc", "--die-with-parent", "--", "bash", "-c", "echo hi"}
	assertEqual(t, "read-only", ro, want)

	ww := bwrapArgs(proto.LevelWrite, "/ws", []string{"/home/u/.cache"}, []string{"/ws/.git"}, argv)
	want = []string{"bwrap", "--ro-bind", "/", "/", "--dev", "/dev", "--proc", "/proc", "--die-with-parent",
		"--tmpfs", "/tmp", "--bind", "/ws", "/ws", "--bind", "/home/u/.cache", "/home/u/.cache",
		"--ro-bind", "/ws/.git", "/ws/.git",
		"--", "bash", "-c", "echo hi"}
	assertEqual(t, "workspace-write", ww, want)

	// level 3/4/9 与 2 同语义（审批通过不豁免沙箱）
	for _, lv := range []int{proto.LevelDanger, 4, proto.LevelApproved} {
		got := bwrapArgs(lv, "/ws", nil, nil, argv)
		if !contains(got, "--bind", "/ws", "/ws") {
			t.Fatalf("bwrapArgs(%d) missing workspace bind: %v", lv, got)
		}
	}
	// 空 workdir / 空 cache / 空 protected 不产出写绑定
	// （--ro-bind / / 是基础只读挂载，恒有）
	got := bwrapArgs(proto.LevelWrite, "", nil, nil, argv)
	if contains(got, "--bind") {
		t.Fatalf("bwrapArgs empty inputs produced write bind: %v", got)
	}
}

// seatbelt 包装：read-only 只含 deny + /dev/null；workspace-write 追加
// 平台临时区 + 工作区 + 缓存目录（全部 canonicalize）+ .git deny 覆盖；
// git 自身豁免 .git 覆盖。
func TestSeatbeltArgs(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix path semantics")
	}
	argv := []string{"bash", "-c", "echo hi"}

	ro := seatbeltArgs(proto.LevelRead, "/ws", argv)
	profile := ro[2]
	if !strings.Contains(profile, "(deny file-write*)") {
		t.Fatalf("read-only profile missing deny: %s", profile)
	}
	if strings.Contains(profile, "subpath") {
		t.Fatalf("read-only profile should have no subpath: %s", profile)
	}
	if ro[0] != macosSeatbeltExecutable || ro[1] != "-p" || ro[3] != "--" {
		t.Fatalf("unexpected seatbelt argv head: %v", ro[:4])
	}

	ww := seatbeltArgs(proto.LevelWrite, "/ws", argv)
	profile = ww[2]
	for _, want := range []string{"/private/tmp", "/ws"} {
		if !strings.Contains(profile, `(subpath "`+want+`")`) {
			t.Fatalf("workspace-write profile missing %q: %s", want, profile)
		}
	}
	// .git 敏感子路径 deny 覆盖（SBPL deny 优先于 allow）
	if !strings.Contains(profile, `(deny file-write* (subpath "`+canonicalRoot("/ws/.git")+`"))`) {
		t.Fatalf("workspace-write profile missing .git deny: %s", profile)
	}
	// git 自身豁免 .git 覆盖（保护对象是 bash/rm 等通用命令）
	gw := seatbeltArgs(proto.LevelWrite, "/ws", []string{"git", "commit", "-m", "x"})
	if strings.Contains(gw[2], "deny file-write* (subpath") {
		t.Fatalf("git invocation should be exempt from .git deny: %s", gw[2])
	}
	// read-only 无 .git deny（无可写根可覆盖）
	if strings.Contains(ro[2], "deny file-write* (subpath") {
		t.Fatalf("read-only profile should not carry subpath deny: %s", ro[2])
	}
}

// writableRoots 去重 + 空值过滤 + canonicalize（/tmp → /private/tmp on darwin）
// + 缓存目录并入（cacheRoots 按机器实际存在的目录产出，不断言具体条目）。
func TestWritableRoots(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix path semantics")
	}
	roots := writableRoots("")
	seen := map[string]bool{}
	for _, r := range roots {
		if r == "" {
			t.Fatalf("empty root leaked: %v", roots)
		}
		if seen[r] {
			t.Fatalf("duplicate root %q: %v", r, roots)
		}
		seen[r] = true
	}
	// /tmp 与 /private/tmp 是同一文件系统身份，canonicalize 后必合并
	if !seen["/private/tmp"] {
		t.Fatalf("/private/tmp missing from roots: %v", roots)
	}
	if len(roots) < 2 { // /private/tmp + os.TempDir()
		t.Fatalf("unexpected roots: %v", roots)
	}
}

// isGitArgv 只认 argv[0] basename 为 git；shell 包装不豁免。
func TestIsGitArgv(t *testing.T) {
	for _, argv := range [][]string{
		{"git", "status"}, {"git", "checkout", "main"}, {"/usr/bin/git", "log"},
	} {
		if !isGitArgv(argv) {
			t.Fatalf("isGitArgv(%v) = false, want true", argv)
		}
	}
	for _, argv := range [][]string{
		{"bash", "-c", "git commit"}, {"sh", "-c", "rm -rf .git"}, {"git-lfs", "pull"}, nil,
	} {
		if isGitArgv(argv) {
			t.Fatalf("isGitArgv(%v) = true, want false", argv)
		}
	}
}

// sbplString 转义反斜杠与引号。
func TestSbplString(t *testing.T) {
	got := sbplString(`C:\a"b`)
	if got != `"C:\\a\"b"` {
		t.Fatalf("sbplString = %s", got)
	}
}

func assertEqual(t *testing.T, name string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: got %v, want %v", name, got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("%s: got %v, want %v", name, got, want)
		}
	}
}

func contains(s []string, sub ...string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		ok := true
		for j := range sub {
			if s[i+j] != sub[j] {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}
