package fsauth

import (
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
	p := New()
	p.SetWorkDir(t.TempDir())
	for _, roots := range [][]string{p.baseRoots, p.decideCaches} {
		for _, r := range roots {
			if r == "" || r == "/" || r == "/**" {
				t.Fatalf("empty/root entry leaked into roots (%v): %q", roots, r)
			}
		}
	}
	if got := joinPattern("", "**"); got != "" {
		t.Fatalf("joinPattern(%q, %q) = %q, want empty", "", "**", got)
	}

	// 正向对照：显式设置 GOCACHE（未 canonical 的 t.TempDir 路径）时必须出现。
	cache := filepath.Join(t.TempDir(), "gocache")
	t.Setenv("GOCACHE", cache)
	p2 := New()
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
	p := New()
	p.SetWorkDir(t.TempDir())
	roots := p.WriteRootsFor("")
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
	temporary := append(tempRoots(), canonical(os.TempDir()))
	roots := p.baseRoots[:0]
	for _, root := range p.baseRoots {
		isTemporary := false
		for _, tmp := range temporary {
			if canonical(root) == canonical(tmp) {
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

// newTestPolicy 构造隔离 Policy（公共区/会话区指向测试根，不碰真实 $HOME/.aic）。
// 直接设字段后必须 rebuildBaseRootsLocked（预计算基底，同 New/SetWorkDir 语义）。
func newTestPolicy(t *testing.T, workDir string) *Policy {
	t.Helper()
	base := mkBase(t)
	p := &Policy{
		workDir:    canonical(workDir),
		publicDir:  canonical(filepath.Join(base, ".aic")),
		sessionDir: canonical(filepath.Join(base, ".aic", "sessions")),
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
func builtinRules() []fsRule {
	out := []fsRule{}
	for _, d := range defaultDenyPaths() {
		if r, ok := compileFSRule("deny:"+d, "builtin"); ok {
			out = append(out, r)
		}
	}
	return out
}

// compileRows 编译给定规则行（非法行跳过，同 rebuildLocked 防御语义）。
func compileRows(rows ...string) []fsRule {
	out := []fsRule{}
	for _, raw := range rows {
		if r, ok := compileFSRule(raw, "cfg"); ok {
			out = append(out, r)
		}
	}
	return out
}

// setRules 重设规则表（包内测试 helper；builtin 初始表 + 给定行按序拼接，
// 同 rebuildLocked 拼接语义；行需带效果前缀，如 "deny:/x/**"、"rw:/y"）。
func setRules(t *testing.T, p *Policy, rows ...string) {
	t.Helper()
	defer isolateTemporaryRoots(p)
	p.mu.Lock()
	defer p.mu.Unlock()
	p.rules = append(builtinRules(), compileRows(rows...)...)
}

// TestDenyPatterns：预展开 deny 模式快照（exec 沙箱读拒绝单源）——
// 默认表含 `.ssh` 展开到 home 绝对路径（变量已展开、字面前缀 canonical），
// 且与断言相同为同一份列表（快照语义，调用方修改不影响 Policy）。
func TestDenyPatterns(t *testing.T) {
	p := newTestPolicy(t, "")
	pats := p.DenyPatterns()
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
	if got := p.DenyPatterns()[0]; got == "__mutated__" {
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
	// 白名单：会话区（per-sid）；注意公共区白名单 = 整个 .aic（含 sessions/），
	// 其他 sid 的会话目录同为 2 级（host 上 per-session 仅是组织约定，非权限边界）
	assertGradesSid(t, p, "s1", p.sessionDir+"/s1/out.txt", 1, 2)
	assertGradesSid(t, p, "s1", p.sessionDir+"/s2/out.txt", 1, 2)
	// 白名单：公共区
	assertGrades(t, p, p.publicDir+"/x.txt", 1, 2)
	// 未匹配：1/0（默认可读，不可写）
	assertGrades(t, p, base+"/elsewhere/f.txt", 1, 0)
	// deny：/** 语义含根自身——连 ls 目录一并拒
	setRules(t, p, "deny:"+base+"/secrets/**")
	assertGrades(t, p, base+"/secrets/key.pem", 0, 0)
	assertGrades(t, p, base+"/secrets", 0, 0)
	assertGrades(t, p, base+"/secrets-sub/x", 1, 0) // 前缀不同名不命中 deny（默认可读不可写）
}

// TestOrderedLastMatchWins：有序表核心语义——优先级即书写顺序。
// 后置 rw 行给 deny 行开洞合法（cfg 显式选择）；反序则 deny 终局。
func TestOrderedLastMatchWins(t *testing.T) {
	base := mkBase(t)
	p := newTestPolicy(t, "")
	// deny 前置 + rw 后置：洞内可写，洞外仍拒
	setRules(t, p, "deny:"+base+"/secure/**", "rw:"+base+"/secure/cache/**")
	assertGrades(t, p, base+"/secure/cache/x", 1, 2)
	assertGrades(t, p, base+"/secure/key.pem", 0, 0)
	// 反序：deny 终局——会话写根/temp grant 不可放宽，open 姿态也不放宽
	setRules(t, p, "rw:"+base+"/secure/cache/**", "deny:"+base+"/secure/**")
	assertGrades(t, p, base+"/secure/cache/x", 0, 0)
	p.Grant("s1", base+"/secure/cache")
	assertGradesSid(t, p, "s1", base+"/secure/cache/x", 0, 0)
	p.openMode = true
	assertGrades(t, p, base+"/secure/cache/x", 0, 0)
	p.openMode = false
	// Rules() 快照：拼接序与效果/来源标注
	rules := p.Rules()
	if len(rules) < 2 || rules[len(rules)-2].Effect != "rw" || rules[len(rules)-1].Effect != "deny" {
		t.Fatalf("Rules snapshot order/effects broken: %+v", rules[len(rules)-2:])
	}
	if rules[len(rules)-1].Source != "cfg" || rules[0].Source != "builtin" {
		t.Fatalf("Rules snapshot source labels broken")
	}
}

// TestDenyFinalAgainstSessionLayer：deny 终局是 session 层唯一硬底线——
// 临时 grant、便利根与 open 姿态都不可放宽表判定的 deny。
func TestDenyFinalAgainstSessionLayer(t *testing.T) {
	base := mkBase(t)
	p := newTestPolicy(t, "")
	setRules(t, p, "rw:"+base, "deny:"+base+"/secret/**")
	p.Grant("s1", base+"/secret")
	assertGradesSid(t, p, "s1", base+"/secret/key", 0, 0)
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
	for _, root := range p.WriteRootsFor("s1") {
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
	setRules(t, p, "deny:"+base+"/secure/**", "ro:"+base+"/secure/doc")
	assertGrades(t, p, base+"/secure/key", 0, 0)
	assertGrades(t, p, base+"/secure/doc", 1, 0)
	assertGrades(t, p, base+"/secure/doc/sub", 1, 0) // 裸模式覆盖子树
	p.Grant("s1", base+"/secure/doc")
	assertGradesSid(t, p, "s1", base+"/secure/doc", 1, 2)
	assertGradesSid(t, p, "s2", base+"/secure/doc", 1, 0)
	// DenyHit 只认终局：doc 已开洞不再是 deny，key 仍是（host 层 temp grant 校验同源）
	if p.DenyHit(base + "/secure/doc") {
		t.Error("ro hole must not report DenyHit")
	}
	if !p.DenyHit(base + "/secure/key") {
		t.Error("denied path must report DenyHit")
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
		e, ok := expandVars(pat)
		if !ok {
			return false
		}
		return matchPattern(canonicalPattern(e), canonical(e))
	}
	for _, pat := range defaultDenyPaths() {
		if !self(pat) {
			t.Errorf("deny entry %q should match its own expansion", pat)
		}
	}
	// browser state 目录口径：json 与保存流程临时文件（含同等全量 cookie）一并命中，
	p := newTestPolicy(t, "")
	browserDir := mustExpand(t, "$HOME/.aic/.cache/browser")
	for _, path := range []string{
		browserDir + "/browser.json",
		browserDir + "/browser.json.inst3.cli-tmp",
		browserDir + "/browser.json.merge-tmp",
	} {
		if !p.DenyHit(path) {
			t.Errorf("DenyHit(%q) = false, want true (browser state dir scope)", path)
		}
	}
	// browser state deny 不得罩住会话区兄弟路径（目录口径向量）
	p2 := newTestPolicy(t, "")
	if !p2.DenyHit(browserDir + "/x.json") {
		t.Fatal("sanity: browser state dir deny must hit its own subtree")
	}
	if e, _ := expandVars("$HOME/.aic/.cache/browser/**"); matchPattern(canonicalPattern(e),
		canonical(mustExpand(t, "$HOME/.aic/sessions/s1/x.txt"))) {
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
		if !p.DenyHit(path) {
			t.Errorf("DenyHit(%q) = false, want true (tilde entry must hit real home path)", path)
		}
	}
	// 兄弟路径不误伤
	for _, path := range []string{
		home + "/.aws-backup/credentials", // 前缀不同名
		home + "/work/.env.sample",        // 段内 * 不跨后缀？**.env 命中 .env 本身——.env.sample 不命中
	} {
		if p.DenyHit(path) {
			t.Errorf("DenyHit(%q) = true, want false (sibling path must not be denied)", path)
		}
	}
}

// TestDenyUndefinedVarEntrySkipped：未定义变量的条目整条跳过——初始名单已按平台分表，
// 此规则只防御用户 cfg 条目（修复背景：单张跨平台表时代，unix 上 %LOCALAPPDATA% 为空，
// 模式退化成 /Google/Chrome/User Data/** 匹配任意位置的同名路径）。
func TestDenyUndefinedVarEntrySkipped(t *testing.T) {
	r, ok := compileFSRule("deny:%LOCALAPPDATA%/Google/Chrome/User Data/**", "cfg")
	if runtime.GOOS == "windows" {
		if !ok || len(r.pats) == 0 {
			t.Errorf("windows: compiled %v, want >=1 pattern", r.pats)
		}
		return
	}
	if ok {
		t.Errorf("non-windows: dangling %%LOCALAPPDATA%% entry must be skipped, got %v", r.pats)
	}
	// 端到端：任意位置的 Google/Chrome/User Data 路径不得被 deny
	p := newTestPolicy(t, "")
	if p.DenyHit("/home/someone/Google/Chrome/User Data/Default/Cookies") {
		t.Error("dangling windows pattern must not deny arbitrary unix path")
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
	lit, ok1 := compileFSRule("deny:"+litPat, "cfg")
	glob, ok2 := compileFSRule("deny:"+globPat, "cfg")
	if !ok1 || !ok2 {
		t.Fatal("compile failed")
	}
	got := append(append([]string{}, lit.pats...), glob.pats...)
	wantLink := filepath.ToSlash(link) + "/x.txt"
	wantReal := canonical(litPat)
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
	for _, want := range []string{wantReal + "/**", wantLink + "/**", canonicalPattern(mustExpand(t, globPat))} {
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

	// DenyHit：deny 行命中 / 界外失配
	setRules(t, p, "deny:"+base+"/secrets/**")
	if !p.DenyHit(base + "/secrets/x") {
		t.Error("DenyHit should hit deny row")
	}
	if p.DenyHit(ext + "/x") {
		t.Error("DenyHit should miss outside deny")
	}
	// grant 与 deny 终局重叠时 deny 优先（session 层不得放宽表判定的 deny）
	p.Grant("s3", base+"/secrets")
	assertGradesSid(t, p, "s3", base+"/secrets/x", 0, 0)
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
// 修复前递归到根基后 TrimSuffix 得空串/盘符，canonical("") 退化为 "."，
// 产出 "./x" 相对形态（安全敏感 helper 的确定性错误）。
func TestCanonicalMissingTopLevel(t *testing.T) {
	vol := filepath.VolumeName(os.TempDir()) // unix ""；windows "C:"
	missing := vol + string(filepath.Separator) + "aic-nonexistent-xyz"
	if _, err := os.Stat(missing); err == nil {
		t.Skip("unexpectedly exists:", missing)
	}
	want := filepath.ToSlash(missing + string(filepath.Separator) + "x")
	if got := Canonical(missing + string(filepath.Separator) + "x"); got != want {
		t.Errorf("Canonical = %q, want %q", got, want)
	}
	// glob 模式同口径（字面前缀经同一 canonical）
	pat := filepath.ToSlash(missing) + "/**"
	if got := canonicalPattern(pat); got != pat {
		t.Errorf("canonicalPattern = %q, want %q", got, pat)
	}
}

// TestDecideMissingTopLevelConsistency：顶层组件不存在时规则判定仍成立——
// 模式与路径共用同一 canonical，退化形态下两端不得失配（判定向量钉死修复语义）。
func TestDecideMissingTopLevelConsistency(t *testing.T) {
	vol := filepath.VolumeName(os.TempDir())
	missing := filepath.ToSlash(vol + string(filepath.Separator) + "aic-nonexistent-xyz")
	p := newTestPolicy(t, "")
	setRules(t, p, "deny:"+missing+"/secrets/**", "rw:"+missing+"/work")
	assertGrades(t, p, missing+"/secrets/key.pem", 0, 0)
	assertGrades(t, p, missing+"/work/f.txt", 1, 2)
}

// TestWriteRootsFor：沙箱白名单 = 便利根 + rw 行裸模式根 + grant，canonical 去重，跨 session 隔离。
func TestWriteRootsFor(t *testing.T) {
	base := mkBase(t)
	ws := filepath.Join(base, "ws")
	mkdir(t, ws)
	p := newTestPolicy(t, ws)
	setRules(t, p, "rw:"+base+"/custom")
	p.Grant("s1", base+"/granted")

	roots := p.WriteRootsFor("s1")
	has := func(want string) bool {
		for _, r := range roots {
			if r == canonical(want) {
				return true
			}
		}
		return false
	}
	for _, want := range []string{ws, base + "/custom", base + "/granted", p.sessionDir + "/s1", p.publicDir} {
		if !has(want) {
			t.Errorf("WriteRootsFor missing %s: %v", want, roots)
		}
	}
	for _, r := range p.WriteRootsFor("s2") {
		if r == canonical(base+"/granted") {
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
	e, ok := expandVars(s)
	if !ok {
		t.Fatalf("expandVars(%q) failed", s)
	}
	return e
}

func assertGrades(t *testing.T, p *Policy, path string, wantR, wantW int) {
	t.Helper()
	assertGradesSid(t, p, "", path, wantR, wantW)
}

func assertGradesSid(t *testing.T, p *Policy, sid, path string, wantR, wantW int) {
	t.Helper()
	r, w := p.View(sid).Decide(path)
	if r != wantR || w != wantW {
		t.Errorf("Decide(%q, sid=%q) = (%d,%d), want (%d,%d)", path, sid, r, w, wantR, wantW)
	}
}

// TestDecideCachesWithoutStat：Decide 与 bind 对不存在缓存目录的语义分茠——
// 判定側无存在性探测（白名单意图跟目录身份：未装 rust 时写 ~/.cargo 也是 2 级），
// bind 侧要求源存在（WriteRootsFor 不含不存在目录）。
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
	for _, r := range p.WriteRootsFor("s1") {
		if r == missing {
			t.Errorf("WriteRootsFor should exclude missing cache dir %s", missing)
		}
	}
}

// canonical 裸盘符：windows 按盘根展开（EvalSymlinks 裸盘符是盘符当前目录语义，
// 权限判定必须盘根口径）；posix 上 "C:" 是普通相对路径名，不特殊处理。
func TestCanonicalBareDrive(t *testing.T) {
	got := Canonical("C:")
	if runtime.GOOS == "windows" {
		if got != "C:/" {
			t.Errorf("Canonical(C:) = %q, want C:/", got)
		}
	} else if got != "C:" {
		t.Errorf("Canonical(C:) = %q, want C:", got)
	}
	if isBareDrive("C:") != (runtime.GOOS == "windows") {
		t.Errorf("isBareDrive(C:) should be windows-only")
	}
	if isBareDrive("C:/") || isBareDrive("C:x") {
		t.Errorf("isBareDrive should only match bare drive")
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
	if rd, wr := p.decide("", canonical(link)); rd != 1 || wr != 0 {
		t.Fatalf("Decide(link) = %d/%d, want 1/0", rd, wr)
	}
	// unlink 语义：字面路径在可写根内 → 1/2。
	if rd, wr := p.decide("", canonicalNoFollow(link)); rd != 1 || wr != 2 {
		t.Fatalf("DecideNoFollow(link) = %d/%d, want 1/2", rd, wr)
	}
	// 父链穿越仍拒：可写根内的目录链接指向根外，其子路径按解析后的父目录判定。
	dirLink := filepath.Join(ws, "dirlink")
	if err := os.Symlink(outside, dirLink); err != nil {
		t.Fatal(err)
	}
	if rd, wr := p.decide("", canonicalNoFollow(filepath.Join(dirLink, "f.txt"))); rd != 1 || wr != 0 {
		t.Fatalf("DecideNoFollow(dirlink/f.txt) = %d/%d, want 1/0 (父链解析到根外)", rd, wr)
	}
	// deny 终局仍压过 unlink 判定：deny 命中的字面路径照旧 0/0。
	denied := filepath.Join(base, "secrets")
	mkdir(t, denied)
	setRules(t, p, "rw:"+ws, "deny:"+denied+"/**")
	if rd, wr := p.decide("", canonicalNoFollow(filepath.Join(denied, "key.pem"))); rd != 0 || wr != 0 {
		t.Fatalf("DecideNoFollow(denied) = %d/%d, want 0/0", rd, wr)
	}
}
