package fsauth

import (
	"github.com/veypi/vbox"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/veypi/aic-pod/cfg"
)

// mkBase 始终使用测试临时目录，不依赖 /var/tmp 可写或宿主机目录布局。
func mkBase(t *testing.T) string {
	t.Helper()
	return t.TempDir()
}

// TestEmptyRootsNeverMatchAll 守住「空 root → /** → 匹配全部路径」这类展开：
// GOCACHE/XDG_CACHE_HOME 未设置时空缓存根不得让判定根/写根退化为全盘
// （2026-09-22：曾使读放行名单失效）。
func TestEmptyRootsNeverMatchAll(t *testing.T) {
	t.Setenv("GOCACHE", "")
	t.Setenv("XDG_CACHE_HOME", "")
	p := mustNewPolicy(t)
	p.SetWorkDir(t.TempDir())
	for _, roots := range [][]string{p.baseRoots, p.decideCaches} {
		for _, r := range roots {
			if r == "" || r == "/" || r == "/**" {
				t.Fatalf("empty/root entry leaked into roots (%v): %q", roots, r)
			}
		}
	}
	if got := vbox.JoinPattern("", "**"); got != "" {
		t.Fatalf("vbox.JoinPattern(%q, %q) = %q, want empty", "", "**", got)
	}

	// 正向对照：显式设置 GOCACHE（未 canonical 的 t.TempDir 路径）时必须出现。
	cache := filepath.Join(t.TempDir(), "gocache")
	t.Setenv("GOCACHE", cache)
	p2 := mustNewPolicy(t)
	p2.SetWorkDir(t.TempDir())
	found := false
	for _, pat := range p2.decideCaches {
		if strings.HasPrefix(pat, cache) {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("GOCACHE dir %q missing from decide roots: %#v", cache, p2.decideCaches)
	}
}

// TestWriteRootsCoverLiteralSpellings：沙箱按系统调用实际传入的路径串匹配，
// /tmp、$TMPDIR(/var/folders/…) 是 symlink 前缀，写名单里必须同时有 canonical 形
// 与字面形（2026-09-22 与双拼写修复同批）。
func TestWriteRootsCoverLiteralSpellings(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("darwin symlink spellings")
	}
	p := mustNewPolicy(t)
	p.SetWorkDir(t.TempDir())
	roots := rulePatterns(p, "", vbox.EffRW)
	for _, want := range []string{"/tmp", os.TempDir()} {
		want = strings.TrimSuffix(want, "/")
		found := false
		for _, r := range roots {
			if r == want {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("write roots missing literal spelling %q: %#v", want, roots)
		}
	}
}

// 隔离测试只移除全局临时区便利根，避免 t.TempDir() 中的拒绝向量被放行。
// 工作区、显式 allow、deny、grant 和缓存等仍由真实实现构造和判定。
func isolateTemporaryRoots(p *Policy) {
	p.mu.Lock()
	defer p.mu.Unlock()
	temporary := append(tempRoots(), vbox.Canonical(os.TempDir()))
	roots := p.baseRoots[:0]
	for _, root := range p.baseRoots {
		isTemporary := false
		for _, tmp := range temporary {
			if vbox.Canonical(root) == vbox.Canonical(tmp) {
				isTemporary = true
				break
			}
		}
		if !isTemporary {
			roots = append(roots, root)
		}
	}
	p.baseRoots = roots
}

func TestDefaultTemporaryRootsRemainWritable(t *testing.T) {
	p := &Policy{grants: map[string][]string{}}
	p.rebuildBaseRootsLocked()
	assertGrades(t, p, filepath.Join(t.TempDir(), "file"), 1, 2)
}

// newTestPolicy 构造隔离 Policy（状态根/会话区指向测试根，不碰真实 $HOME/.aic）。
// 直接设字段后必须 rebuildBaseRootsLocked（预计算基底，同 New/SetWorkDir 语义）。
func newTestPolicy(t *testing.T, workDir string) *Policy {
	t.Helper()
	base := mkBase(t)
	p := &Policy{
		workDir:    vbox.Canonical(workDir),
		stateDir:   vbox.Canonical(filepath.Join(base, ".aic")),
		sessionDir: vbox.Canonical(filepath.Join(base, ".aic", "sessions")),
		grants:     map[string][]string{},
	}
	mkdir(t, p.sessionDir)
	p.rules = builtinRules()
	p.mu.Lock()
	p.rebuildBaseRootsLocked()
	p.mu.Unlock()
	isolateTemporaryRoots(p)
	return p
}

// builtinRules 编译出厂初始表（同 rebuildLocked 的 builtin 段）。
func builtinRules() []vbox.Rule {
	rows := defaultDenyPaths()
	for i := range rows {
		rows[i] = "deny:" + rows[i]
	}
	rules, err := vbox.CompileFSRules(rows, vbox.ClassBuiltin)
	if err != nil {
		panic(err)
	}
	return rules
}
func compileRows(rows ...string) []vbox.Rule {
	rules, err := vbox.CompileFSRules(rows, vbox.ClassCfg)
	if err != nil {
		panic(err)
	}
	return rules
}

// setRules 重设规则表（包内测试 helper；builtin 初始表 + 给定行按序拼接，
// 同 rebuildLocked 拼接语义；行需带效果前缀，如 "deny:/x/**"、"rw:/y"）。
func setRules(t *testing.T, p *Policy, rows ...string) {
	t.Helper()
	defer isolateTemporaryRoots(p)
	p.mu.Lock()
	defer p.mu.Unlock()
	p.rules = append(compileRows(rows...), builtinRules()...)
}

// TestDenyPatterns：预展开 deny 模式快照（exec 沙箱读拒绝单源）——
// 默认表含 `.ssh` 展开到 home 绝对路径（变量已展开、字面前缀 canonical），
// 且与断言相同为同一份列表（快照语义，调用方修改不影响 Policy）。
func TestDenyPatterns(t *testing.T) {
	p := newTestPolicy(t, "")
	pats := rulePatterns(p, "", vbox.EffDeny)
	if len(pats) == 0 {
		t.Fatalf("default deny patterns must not be empty")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	foundSSH := false
	for _, pat := range pats {
		if pat == "**/.ssh/**" || pat == home+"/.ssh/**" {
			foundSSH = true
		}
	}
	if !foundSSH {
		t.Fatalf("deny patterns missing .ssh entry: %v", pats)
	}
	// 快照：修改返回切片不影响 Policy 内部表
	pats[0] = "__mutated__"
	if got := rulePatterns(p, "", vbox.EffDeny)[0]; got == "__mutated__" {
		t.Fatalf("DenyPatterns must return a copy")
	}
}

// TestDecideGrading：deny → 0/0；可写白名单 → 1/2；未匹配 → 1/0（默认可读不可写）。
func TestDecideGrading(t *testing.T) {
	base := mkBase(t)
	ws := filepath.Join(base, "ws")
	mkdir(t, ws)
	p := newTestPolicy(t, ws)

	// 白名单：work_dir
	assertGrades(t, p, ws+"/code/x.go", 1, 2)
	// 白名单：会话区（per-sid）；其他 sid 的会话目录不可写——目录两根治理后
	// .aic 整体退出写白名单，sessions/{sid} 由 Snapshot write rules 按会话单独授写。
	assertGradesSid(t, p, "s1", p.sessionDir+"/s1/out.txt", 1, 2)
	assertGradesSid(t, p, "s1", p.sessionDir+"/s2/out.txt", 1, 0)
	// 设备状态根只读：.aic 不再是公共可写区（2026-10-01 目录两根治理）
	assertGrades(t, p, p.stateDir+"/x.txt", 1, 0)
	// 未匹配：1/0（默认可读，不可写）
	assertGrades(t, p, base+"/elsewhere/f.txt", 1, 0)
	// deny：/** 语义含根自身——连 ls 目录一并拒
	setRules(t, p, "deny:"+base+"/secrets/**")
	assertGrades(t, p, base+"/secrets/key.pem", 0, 0)
	assertGrades(t, p, base+"/secrets", 0, 0)
	assertGrades(t, p, base+"/secrets-sub/x", 1, 0) // 前缀不同名不命中 deny（默认可读不可写）
}

// TestOrderedFirstMatchWins：有序表核心语义——优先级即书写顺序。
// 后置 rw 行给 deny 行开洞合法（cfg 显式选择）；反序则 deny 终局。
func TestOrderedFirstMatchWins(t *testing.T) {
	base := mkBase(t)
	p := newTestPolicy(t, "")
	// deny 前置 + rw 后置：洞内可写，洞外仍拒
	setRules(t, p, "rw:"+base+"/secure/cache/**", "deny:"+base+"/secure/**")
	assertGrades(t, p, base+"/secure/cache/x", 1, 2)
	assertGrades(t, p, base+"/secure/key.pem", 0, 0)
	// 反序：deny 终局——便利根/open 姿态不可放宽；temp grant 插表头可覆盖
	//（v4 新语义，2.7.4「用户点就点了」）
	setRules(t, p, "deny:"+base+"/secure/**", "rw:"+base+"/secure/cache/**")
	assertGrades(t, p, base+"/secure/cache/x", 0, 0)
	p.Grant("s1", base+"/secure/cache")
	assertGradesSid(t, p, "s1", base+"/secure/cache/x", 1, 2)
	// 会话隔离：别的 sid 仍被 deny
	assertGradesSid(t, p, "s2", base+"/secure/cache/x", 0, 0)
	p.openMode = true
	assertGrades(t, p, base+"/secure/cache/x", 0, 0)
	p.openMode = false
	rows := p.Snapshot("").Rules
	if len(rows) < 2 || rows[0].Effect != vbox.EffDeny || rows[0].Class != vbox.ClassCfg {
		t.Fatalf("snapshot order: %+v", rows)
	}

}

// TestDenyFinalExceptTempGrant：deny 终局对便利根与 open 姿态仍是硬约束；
// 临时 grant 插表头可覆盖（v4 新语义，2.7.4「用户点就点了」——旧 session
// 硬底线作废）。
func TestDenyFinalExceptTempGrant(t *testing.T) {
	base := mkBase(t)
	p := newTestPolicy(t, "")
	setRules(t, p, "deny:"+base+"/secret/**", "rw:"+base)
	p.Grant("s1", base+"/secret")
	assertGradesSid(t, p, "s1", base+"/secret/key", 1, 2)
	// 会话隔离：别的 sid 仍被 deny
	assertGradesSid(t, p, "s2", base+"/secret/key", 0, 0)
	assertGrades(t, p, base+"/ordinary", 1, 2)
	p.openMode = true
	assertGrades(t, p, base+"/secret/key", 0, 0)
}

// TestAllowWriteAndRevocation：rw 行只授予写；未命中路径默认可读不可写；
// grant 按 session 隔离，DropSession 撤销。
func TestAllowWriteAndRevocation(t *testing.T) {
	base := mkBase(t)
	p := newTestPolicy(t, "")
	old := cfg.AuthSnapshot()
	t.Cleanup(func() { cfg.SetAuth(old) })
	a := old
	a.FsPolicy = "deny"
	a.FsRules = []string{"rw:" + base + "/write"}
	cfg.SetAuth(a)
	p.Reconcile()
	isolateTemporaryRoots(p)
	assertGrades(t, p, base+"/read/file", 1, 0)
	assertGrades(t, p, base+"/write/file", 1, 2)
	assertGrades(t, p, base+"/outside/file", 1, 0)
	for _, root := range rulePatterns(p, "s1", vbox.EffRW) {
		if root == base+"/read" {
			t.Fatal("non-allow root became writable")
		}
	}
	p.Grant("s1", base+"/outside")
	assertGradesSid(t, p, "s1", base+"/outside/file", 1, 2)
	assertGradesSid(t, p, "s2", base+"/outside/file", 1, 0)
	p.DropSession("s1")
	assertGradesSid(t, p, "s1", base+"/outside/file", 1, 0)
}

// TestROHoleSemantics：ro 行的唯一语义价值是从上方 deny 行开读洞——
// 洞内读开放写仍拒；temp grant 可把 ro 目标提升为可写（resolve≠deny，§3 预期固化）。
func TestROHoleSemantics(t *testing.T) {
	base := mkBase(t)
	p := newTestPolicy(t, "")
	setRules(t, p, "ro:"+base+"/secure/doc", "deny:"+base+"/secure/**")
	assertGrades(t, p, base+"/secure/key", 0, 0)
	assertGrades(t, p, base+"/secure/doc", 1, 0)
	assertGrades(t, p, base+"/secure/doc/sub", 1, 0) // 裸模式覆盖子树
	p.Grant("s1", base+"/secure/doc")
	assertGradesSid(t, p, "s1", base+"/secure/doc", 1, 2)
	assertGradesSid(t, p, "s2", base+"/secure/doc", 1, 0)
	// deniedPath 只认终局：doc 已开洞不再是 deny，key 仍是
	if denied(p, base+"/secure/doc") {
		t.Error("ro hole must not report deniedPath")
	}
	if !denied(p, base+"/secure/key") {
		t.Error("denied path must report deniedPath")
	}
	// open 姿态下 ro 是写的唯一约束（§8.5）
	p.openMode = true
	assertGrades(t, p, base+"/secure/doc", 1, 0)
	assertGrades(t, p, base+"/anywhere", 1, 2)
}

// TestDenyDefaults：平台初始表字面形态自洽（逐条遍历本平台生效表：
// expandVars + canonicalPattern 后自匹配，且不误伤兄弟路径）。
func TestDenyDefaults(t *testing.T) {
	self := func(pat string) bool {
		e, ok := vbox.ExpandVars(pat)
		if !ok {
			return false
		}
		return vbox.MatchPattern(vbox.CanonicalPattern(e), vbox.Canonical(e))
	}
	for _, pat := range defaultDenyPaths() {
		if !self(pat) {
			t.Errorf("deny entry %q should match its own expansion", pat)
		}
	}
	// browser state 目录口径：json 与保存流程临时文件（含同等全量 cookie）一并命中，
	p := newTestPolicy(t, "")
	browserDir := mustExpand(t, "$HOME/.aic/browser")
	for _, path := range []string{
		browserDir + "/browser.json",
		browserDir + "/browser.json.inst3.cli-tmp",
		browserDir + "/browser.json.merge-tmp",
	} {
		if !denied(p, path) {
			t.Errorf("deniedPath(%q) = false, want true (browser state dir scope)", path)
		}
	}
	// browser state deny 不得罩住会话区兄弟路径（目录口径向量）
	p2 := newTestPolicy(t, "")
	if !denied(p2, browserDir+"/x.json") {
		t.Fatal("sanity: browser state dir deny must hit its own subtree")
	}
	if e, _ := vbox.ExpandVars("$HOME/.aic/browser/**"); vbox.MatchPattern(vbox.CanonicalPattern(e),
		vbox.Canonical(mustExpand(t, "$HOME/.aic/sessions/s1/x.txt"))) {
		t.Error("browser state dir deny must not shadow session files")
	}
}

// TestDenyTildeEntries：~ 条目预展开后命中真实家目录下的凭证路径——
// 修复前 expandVars 不展开 ~，~/.aws/** 等条目全部死模式（由 **/.ssh/** 掩盖未暴露）。
func TestDenyTildeEntries(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("home layout 向量以 unix 为主")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir")
	}
	p := newTestPolicy(t, "")
	paths := []string{
		home + "/.aws/credentials",
		home + "/.aws/config",
		home + "/.config/gcloud/access_tokens.db",
		home + "/.azure/msal_token_cache.json",
		home + "/.netrc",
		home + "/.npmrc",
		home + "/.docker/config.json",
		home + "/.claude/.credentials.json",
		home + "/.config/gh/hosts.yml",
		home + "/.gnupg/private-keys-v1.d/x.key",
		// shell 历史与容器 socket（通配形态跨平台命中）
		home + "/.zsh_history",
		home + "/.bash_history",
		home + "/.python_history",
		"/var/run/docker.sock",
	}
	if runtime.GOOS == "darwin" {
		paths = append(paths,
			home+"/Library/Keychains/login.keychain-db",
			home+"/Library/Cookies/Cookies.binarycookies",
			home+"/Library/Application Support/Firefox/Profiles/x/key4.db",
			home+"/.orbstack/run/docker.sock",
			home+"/.orbstack/run/sconssh.sock",
			home+"/.local/share/containers/podman/machine/podman.sock",
		)
	}
	if runtime.GOOS == "linux" {
		paths = append(paths,
			home+"/.mozilla/firefox/x.default/key4.db",
			home+"/.config/chromium/Default/Cookies",
			home+"/.local/share/keyrings/login.keyring",
			home+"/.local/share/containers/podman/machine/podman.sock",
		)
	}
	for _, path := range paths {
		if !denied(p, path) {
			t.Errorf("deniedPath(%q) = false, want true (tilde entry must hit real home path)", path)
		}
	}
	// 兄弟路径不误伤
	for _, path := range []string{
		home + "/.aws-backup/credentials", // 前缀不同名
		home + "/work/.env.sample",        // 段内 * 不跨后缀？**.env 命中 .env 本身——.env.sample 不命中
	} {
		if denied(p, path) {
			t.Errorf("deniedPath(%q) = true, want false (sibling path must not be denied)", path)
		}
	}
}

// TestDenyUndefinedVarEntrySkipped：未定义变量的条目整条跳过——初始名单已按平台分表，
// 此规则只防御用户 cfg 条目（修复背景：单张跨平台表时代，unix 上 %LOCALAPPDATA% 为空，
// 模式退化成 /Google/Chrome/User Data/** 匹配任意位置的同名路径）。
func TestDenyUndefinedVarRejectsTable(t *testing.T) {
	if _, err := vbox.CompileFSRules([]string{"deny:$AIC_UNDEFINED_POLICY_VAR/**"}, vbox.ClassCfg); err == nil {
		t.Fatal("undefined variable accepted")
	}
}

// TestCompileRuleDualForm：纯字面条目输出双形态（canonical + 原始展开形）——
// 模式自身是符号链接时（/var/run/docker.sock → 厂商 socket 形态）两形态是
// 不同的路径串，沙箱按实际传入路径匹配，双形态都须在名单内（实测 2026-09-05）。
// 裸模式 ≡ 覆盖整棵子树（两拼写各带 /** 形态）；glob 条目仅输出 canonical 单形态。
func TestCompileRuleDualForm(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "real")
	mkdir(t, real)
	link := filepath.Join(base, "alias")
	if err := os.Symlink(real, link); err != nil {
		t.Skip("symlink unavailable:", err)
	}
	litPat := filepath.ToSlash(link) + "/x.txt"
	globPat := filepath.ToSlash(link) + "/**"
	lit, err1 := vbox.CompileFSRules([]string{"deny:" + litPat}, vbox.ClassCfg)
	glob, err2 := vbox.CompileFSRules([]string{"deny:" + globPat}, vbox.ClassCfg)
	if err1 != nil || err2 != nil {
		t.Fatal("compile failed")
	}
	var got []string
	for _, row := range append(lit, glob...) {
		got = append(got, row.Pattern)
	}
	wantLink := filepath.ToSlash(link) + "/x.txt"
	wantReal := vbox.Canonical(litPat)
	if wantReal == wantLink {
		t.Skip("symlink not resolved on this platform")
	}
	hasLink, hasReal := false, false
	for _, g := range got {
		if g == wantLink {
			hasLink = true
		}
		if g == wantReal {
			hasReal = true
		}
	}
	if !hasLink || !hasReal {
		t.Fatalf("dual form missing: got %v, want both %q and %q", got, wantLink, wantReal)
	}
	// 裸模式覆盖子树（双拼写）+ glob 条目 canonical 形态
	for _, want := range []string{wantReal, wantLink, vbox.CanonicalPattern(mustExpand(t, globPat))} {
		found := false
		for _, g := range got {
			if g == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("missing pattern %q: %v", want, got)
		}
	}
}

// TestGrantTemp：临时 grant（canonical 前缀、幂等、跨 session 失效、deny 校验）。
func TestGrantTemp(t *testing.T) {
	base := mkBase(t)
	ext := filepath.Join(base, "ext")
	mkdir(t, ext)
	p := newTestPolicy(t, "")

	assertGradesSid(t, p, "s1", ext+"/a.txt", 1, 0)
	p.Grant("s1", ext)
	p.Grant("s1", ext) // 幂等
	assertGradesSid(t, p, "s1", ext+"/a.txt", 1, 2)
	// 跨 session 失效（写撤销，读仍在）
	assertGradesSid(t, p, "s2", ext+"/a.txt", 1, 0)

	// deniedPath：deny 行命中 / 界外失配
	setRules(t, p, "deny:"+base+"/secrets/**")
	if !denied(p, base+"/secrets/x") {
		t.Error("deniedPath should hit deny row")
	}
	if denied(p, ext+"/x") {
		t.Error("deniedPath should miss outside deny")
	}
	// grant 插表头压 deny（v4 新语义——旧「deny 优先」硬底线作废）；会话隔离
	p.Grant("s3", base+"/secrets")
	assertGradesSid(t, p, "s3", base+"/secrets/x", 1, 2)
	assertGradesSid(t, p, "s4", base+"/secrets/x", 0, 0)
}

// TestCanonicalSymlinkBypass：canonical 判定防 symlink 绕过（评审必修项向量）。
func TestCanonicalSymlinkBypass(t *testing.T) {
	base := mkBase(t)
	ws := filepath.Join(base, "ws")
	outside := filepath.Join(base, "outside")
	mkdir(t, ws, outside)
	p := newTestPolicy(t, ws)

	link := filepath.Join(ws, "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Skip("symlink unavailable:", err)
	}
	// 经 link 访问 outside 下的文件 → canonical 后落在 outside（非白名单）→
	// 1/0：canonical 判定防 symlink 写出白名单。
	assertGrades(t, p, link+"/f.txt", 1, 0)
	// link 自身的 canonical 身份 = outside（EvalSymlinks 成功）→ 同样 1/0：
	// 写 link 即写 outside，权限随真实目标
	assertGrades(t, p, link, 1, 0)
	// 白名单内普通文件不受影响
	assertGrades(t, p, ws+"/f.txt", 1, 2)
}

// TestCanonicalMissingTopLevel：顶层组件不存在的路径保持绝对形态。
// 修复前递归到根基后 TrimSuffix 得空串/盘符，vbox.Canonical("") 退化为 "."，
// 产出 "./x" 相对形态（安全敏感 helper 的确定性错误）。
func TestCanonicalMissingTopLevel(t *testing.T) {
	vol := filepath.VolumeName(os.TempDir()) // unix ""；windows "C:"
	missing := vol + string(filepath.Separator) + "aic-nonexistent-xyz"
	if _, err := os.Stat(missing); err == nil {
		t.Skip("unexpectedly exists:", missing)
	}
	want := filepath.ToSlash(missing + string(filepath.Separator) + "x")
	if got := vbox.Canonical(missing + string(filepath.Separator) + "x"); got != want {
		t.Errorf("Canonical = %q, want %q", got, want)
	}
	// glob 模式同口径（字面前缀经同一 canonical）
	pat := filepath.ToSlash(missing) + "/**"
	if got := vbox.CanonicalPattern(pat); got != pat {
		t.Errorf("canonicalPattern = %q, want %q", got, pat)
	}
}

// TestDecideMissingTopLevelConsistency：顶层组件不存在时规则判定仍成立——
// 模式与路径共用同一 canonical，退化形态下两端不得失配（判定向量钉死修复语义）。
func TestDecideMissingTopLevelConsistency(t *testing.T) {
	vol := filepath.VolumeName(os.TempDir())
	missing := filepath.ToSlash(vol + string(filepath.Separator) + "aic-nonexistent-xyz")
	p := newTestPolicy(t, "")
	setRules(t, p, "rw:"+missing+"/work", "deny:"+missing+"/secrets/**")
	assertGrades(t, p, missing+"/secrets/key.pem", 0, 0)
	assertGrades(t, p, missing+"/work/f.txt", 1, 2)
}

// TestSnapshotWriteRules：沙箱白名单 = 便利根 + rw 行裸模式根 + grant，canonical 去重，跨 session 隔离。
func TestSnapshotWriteRules(t *testing.T) {
	base := mkBase(t)
	ws := filepath.Join(base, "ws")
	mkdir(t, ws)
	p := newTestPolicy(t, ws)
	setRules(t, p, "rw:"+base+"/custom")
	p.Grant("s1", base+"/granted")

	roots := rulePatterns(p, "s1", vbox.EffRW)
	has := func(want string) bool {
		for _, r := range roots {
			if r == vbox.Canonical(want) {
				return true
			}
		}
		return false
	}
	for _, want := range []string{ws, base + "/custom", base + "/granted", p.sessionDir + "/s1"} {
		if !has(want) {
			t.Errorf("Snapshot write rules missing %s: %v", want, roots)
		}
	}
	if has(p.stateDir) {
		t.Error("Snapshot write rules must not grant device state root ($HOME/.aic read-only)")
	}
	for _, r := range rulePatterns(p, "s2", vbox.EffRW) {
		if r == vbox.Canonical(base+"/granted") {
			t.Error("grant leaked across sessions")
		}
	}
}

// TestReconcile：cfg.Global 变更经 Reconcile 重载（set_config 动态生效向量，
// write roots 与 deny 双侧）。
func TestReconcile(t *testing.T) {
	base := mkBase(t)
	custom := filepath.Join(base, "custom")
	secrets := filepath.Join(base, "secrets")
	mkdir(t, custom, secrets)
	p := newTestPolicy(t, "")

	assertGrades(t, p, custom+"/x", 1, 0)
	saved := cfg.Global
	defer func() { cfg.Global = saved }()
	o := cfg.NewOptions()
	o.FsRules = []string{"rw:" + custom, "deny:" + secrets + "/**"}
	cfg.Global = o
	p.Reconcile()
	isolateTemporaryRoots(p)
	assertGrades(t, p, custom+"/x", 1, 2)
	assertGrades(t, p, secrets+"/x", 0, 0)
	// Reconcile 后规则表重建：cfg 清空即恢复出厂表（该路径回到默认可读不可写）
	o2 := cfg.NewOptions()
	cfg.Global = o2
	p.Reconcile()
	isolateTemporaryRoots(p)
	assertGrades(t, p, secrets+"/x", 1, 0)
}

func mkdir(t *testing.T, dirs ...string) {
	t.Helper()
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

func mustExpand(t *testing.T, s string) string {
	t.Helper()
	e, ok := vbox.ExpandVars(s)
	if !ok {
		t.Fatalf("vbox.ExpandVars(%q) failed", s)
	}
	return e
}

func assertGrades(t *testing.T, p *Policy, path string, wantR, wantW int) {
	t.Helper()
	assertGradesSid(t, p, "", path, wantR, wantW)
}

func assertGradesSid(t *testing.T, p *Policy, sid, path string, wantR, wantW int) {
	t.Helper()
	r, w := 0, 0
	snap := p.Snapshot(sid)
	if snap.Match(path, vbox.OpRead).Allow {
		r = 1
	}
	if snap.Match(path, vbox.OpWrite).Allow {
		w = 2
	}
	if r != wantR || w != wantW {
		t.Errorf("Decide(%q, sid=%q) = (%d,%d), want (%d,%d)", path, sid, r, w, wantR, wantW)
	}
}

// TestDecideCachesWithoutStat：Decide 与 bind 对不存在缓存目录的语义分茠——
// 判定側无存在性探测（白名单意图跟目录身份：未装 rust 时写 ~/.cargo 也是 2 级），
// bind 侧要求源存在（Snapshot write rules 不含不存在目录）。
func TestDecideCachesWithoutStat(t *testing.T) {
	var missing string
	for _, d := range cacheRootDirs() {
		if d == "" {
			continue
		}
		if _, err := os.Stat(d); err != nil {
			missing = d
			break
		}
	}
	if missing == "" {
		t.Skip("all cache candidate dirs exist; no missing dir to lock semantics")
	}
	p := newTestPolicy(t, "")
	// Decide：不存在的缓存目录仍 2 级（预计算候选，无 stat）
	assertGrades(t, p, filepath.Join(missing, "pkg"), 1, 2)
	// bind：不存在则不进白名单

}

// canonical 形路径禁止 filepath.Join 回填反斜杠（win \c\… 毒形态回归：
// 该行进 Snapshot write rules 后 win 沙箱 grantDirWrite 必失败）。会话根行任何
// 平台都不得含反斜杠（缓存/临时根的原生态字面行不在此列——只断言行内
// 含 sid 的会话根）。
func TestSessionRootRowsNoBackslash(t *testing.T) {
	p := mustNewPolicy(t)
	if strings.Contains(p.sessionDir, `\`) {
		t.Fatalf("sessionDir = %q", p.sessionDir)
	}
	for _, r := range rulePatterns(p, "s1", vbox.EffRW) {
		if strings.Contains(r, "s1") && strings.Contains(r, `\`) {
			t.Fatalf("decideRoots session row = %q", r)
		}
	}

}

// canonical 盘符输入归一（2026-09-24 /c/ 规范形）：windows 上裸盘符按盘根
// 展开为 /c；posix 上 "C:" 是普通相对路径名，不特殊处理。
func TestCanonicalBareDrive(t *testing.T) {
	got := vbox.Canonical("C:")
	if runtime.GOOS == "windows" {
		if got != "/c" {
			t.Errorf("vbox.Canonical(C:) = %q, want /c", got)
		}
	} else if got != "C:" {
		t.Errorf("vbox.Canonical(C:) = %q, want C:", got)
	}
	if runtime.GOOS == "windows" {
		if got := vbox.Canonical(`C:\Users`); got != "/c/Users" {
			t.Errorf("vbox.Canonical(C:\\Users) = %q, want /c/Users", got)
		}
		if got := vbox.Canonical("/c/Users"); got != "/c/Users" {
			t.Errorf("vbox.Canonical(/c/Users) = %q, want /c/Users", got)
		}
	}
}

// TestDecideNoFollowUnlink：unlink/rename 语义的判定——末段符号链接不跟随。
// 可写根内的外向链接：跟随判定（Decide）因目标在根外拒写；unlink 判定
// （DecideNoFollow）按字面路径放行（删/挪的是链接本身）。父链穿越与 deny 不受影响。
func TestDecideNoFollowUnlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs privileges on windows")
	}
	base := mkBase(t)
	ws := filepath.Join(base, "ws")
	mkdir(t, ws)
	outside := filepath.Join(base, "outside")
	mkdir(t, outside)
	outsideFile := filepath.Join(outside, "target.txt")
	if err := os.WriteFile(outsideFile, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(ws, "link")
	if err := os.Symlink(outsideFile, link); err != nil {
		t.Fatal(err)
	}
	p := newTestPolicy(t, "")
	setRules(t, p, "rw:"+ws)
	// 跟随语义（现状不变）：目标在可写根外 → 写 0。
	assertGrades(t, p, link, 1, 0)
	// unlink 语义：字面路径在可写根内 → 1/2。
	if d := p.Snapshot("").MatchNoFollow(link, vbox.OpWrite); !d.Allow {
		t.Fatal("unlink should check link path")
	}
	// 父链穿越仍拒：可写根内的目录链接指向根外，其子路径按解析后的父目录判定。
	dirLink := filepath.Join(ws, "dirlink")
	if err := os.Symlink(outside, dirLink); err != nil {
		t.Fatal(err)
	}
	if d := p.Snapshot("").MatchNoFollow(filepath.Join(dirLink, "f.txt"), vbox.OpWrite); d.Allow {
		t.Fatal("parent symlink must resolve outside")
	}
	// deny 终局仍压过 unlink 判定：deny 命中的字面路径照旧 0/0。
	denied := filepath.Join(base, "secrets")
	mkdir(t, denied)
	setRules(t, p, "deny:"+denied+"/**", "rw:"+ws)
	if d := p.Snapshot("").MatchNoFollow(filepath.Join(denied, "key.pem"), vbox.OpRead); d.Allow {
		t.Fatal("deny must block link path")
	}
}

func mustNewPolicy(t *testing.T) *Policy {
	t.Helper()
	p, err := New()
	if err != nil {
		t.Fatal(err)
	}
	return p
}
func rulePatterns(p *Policy, sid string, effect vbox.Effect) []string {
	var out []string
	for _, r := range p.Snapshot(sid).Rules {
		if r.Effect == effect {
			out = append(out, r.Pattern)
		}
	}
	return out
}
func denied(p *Policy, target string) bool {
	return p.Snapshot("").Match(target, vbox.OpRead).Effect == vbox.EffDeny
}
