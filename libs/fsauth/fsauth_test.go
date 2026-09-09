package fsauth

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/veypi/aic-pod/cfg"
)

// mkBase 在 os.TempDir() 之外建测试根（t.TempDir() 落在临时区白名单内，
// 分级向量会被污染）：优先 /var/tmp（unix），不可写时回落 t.TempDir()。
// 两侧（模式/路径）都经 canonical 归一，/var → /private/var 类 symlink 安全。
func mkBase(t *testing.T) string {
	t.Helper()
	for _, parent := range []string{"/var/tmp", "/tmp"} {
		if base, err := os.MkdirTemp(parent, "fsauth-"); err == nil {
			t.Cleanup(func() { os.RemoveAll(base) })
			return base
		}
	}
	return t.TempDir()
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
	p.deny = compileDeny(defaultDenyPaths())
	p.mu.Lock()
	p.rebuildBaseRootsLocked()
	p.mu.Unlock()
	return p
}

// setDeny 重设预展开 deny 表（包内测试 helper；默认表 + extra 叠加，同 compileDeny 语义）。
func setDeny(t *testing.T, p *Policy, extra ...string) {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	p.deny = compileDeny(append(defaultDenyPaths(), extra...))
}

// setAllow 重设 fs_allow 显式条目（包内测试 helper；同 rebuildLocked 的 splitAllow 语义：
// 裸路径 → 写白名单根，通配 → 精确 glob）。
func setAllow(t *testing.T, p *Policy, entries ...string) {
	t.Helper()
	roots, globs := splitAllow(entries)
	p.mu.Lock()
	defer p.mu.Unlock()
	p.extraWrite = roots
	p.allowGlobs = globs
	p.rebuildBaseRootsLocked()
}

// addAllowRoot 追加单个裸路径 fs_allow 条目（包内测试 helper；同 rebuildLocked 语义）。
func addAllowRoot(t *testing.T, p *Policy, root string) {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	p.extraWrite = append(p.extraWrite, canonical(root))
	p.rebuildBaseRootsLocked()
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

// TestDecideGrading：deny → 0/0；白名单 → 1/2；其余 → 1/3。
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
	// 其余：1/3
	assertGrades(t, p, base+"/elsewhere/f.txt", 1, 3)
	// deny：/** 语义含根自身——连 ls 目录一并拒
	setDeny(t, p, base+"/secrets/**")
	assertGrades(t, p, base+"/secrets/key.pem", 0, 0)
	assertGrades(t, p, base+"/secrets", 0, 0)
	assertGrades(t, p, base+"/secrets-sub/x", 1, 3) // 前缀不同名不命中
}

// TestDecideAllowOverride：allow 覆盖 deny（两键语义，2026-09-09）——显式 fs_allow
// 条目压过 deny：裸路径覆盖其子树、通配条目精确匹配（并授予写 2）；内建便利根
// （工作区/临时区/公共区/缓存/会话区）与临时 grant 不压（安全默认保持权威）。
func TestDecideAllowOverride(t *testing.T) {
	base := mkBase(t)
	ws := filepath.Join(base, "ws")
	proj := filepath.Join(ws, "proj")
	other := filepath.Join(base, "other")
	allowed := filepath.Join(base, "allowed")
	granted := filepath.Join(base, "granted")
	mkdir(t, proj, other, allowed, granted)
	p := newTestPolicy(t, ws)
	setDeny(t, p, "**/.env", "**/*.pem")

	// 触发点：工作区内的 .env 无差别拒（内建根不压 deny）
	assertGrades(t, p, proj+"/.env", 0, 0)
	assertGrades(t, p, proj+"/cert.pem", 0, 0)
	// 内建便利根同样不压：公共区 / 临时区 / 会话区
	assertGrades(t, p, p.publicDir+"/.env", 0, 0)
	assertGrades(t, p, os.TempDir()+"/.env", 0, 0)
	assertGradesSid(t, p, "s1", p.sessionDir+"/s1/.env", 0, 0)
	// 临时 grant 不压 deny（grant 是 AI 经审批申请的，不构成信任声明）
	p.Grant("s1", granted)
	assertGradesSid(t, p, "s1", granted+"/.env", 0, 0)

	// 通配条目：精确覆盖（只放 .env，同目录 *.pem 仍拒），并授予写 2
	setAllow(t, p, proj+"/**/.env")
	assertGrades(t, p, proj+"/.env", 1, 2)
	assertGrades(t, p, proj+"/sub/.env", 1, 2)
	assertGrades(t, p, proj+"/cert.pem", 0, 0) // 未覆盖的 deny 条目照旧
	assertGrades(t, p, other+"/.env", 0, 0)    // 作用域外照旧
	assertGrades(t, p, ws+"/.env", 0, 0)       // 兄弟目录不在 glob 内
	assertGrades(t, p, proj+"/main.go", 1, 2)

	// 裸路径条目：覆盖其子树（.env 与 *.pem 都放），同时是写白名单根
	setAllow(t, p, allowed)
	assertGrades(t, p, allowed+"/.env", 1, 2)
	assertGrades(t, p, allowed+"/sub/cert.pem", 1, 2)
	assertGrades(t, p, proj+"/.env", 0, 0)

	// 多条目：裸路径 + 通配并存；展开快照只含显式条目
	setAllow(t, p, allowed, proj+"/**/.env")
	assertGrades(t, p, allowed+"/.env", 1, 2)
	assertGrades(t, p, proj+"/.env", 1, 2)
	assertGrades(t, p, proj+"/cert.pem", 0, 0)
	pats := p.DenyOverridePatterns()
	if len(pats) != 2 {
		t.Fatalf("DenyOverridePatterns = %v, want 2 entries (explicit only)", pats)
	}
	wantRoot := canonical(allowed) + "/**"
	wantGlob := canonicalPattern(proj + "/**/.env")
	for _, want := range []string{wantRoot, wantGlob} {
		found := false
		for _, pat := range pats {
			if pat == want {
				found = true
			}
		}
		if !found {
			t.Errorf("DenyOverridePatterns missing %q: %v", want, pats)
		}
	}

	// DenyHit 仍是原始表（grant fs 校验语义：deny 内目标不因 allow 而可 grant）
	if !p.DenyHit(proj + "/.env") {
		t.Error("DenyHit must stay raw (no allow override) for grant validation")
	}

	// 空/未定义变量条目整条跳过
	setAllow(t, p, "", "  ", "$UNDEFINED_VAR_XYZ/x")
	if got := p.DenyOverridePatterns(); got != nil {
		t.Errorf("invalid entries must be skipped, got %v", got)
	}

	// 根为 "/" 的边界：拼接不得产生 "//"（否则模式整体失配）
	p2 := newTestPolicy(t, "")
	setDeny(t, p2, "**/.env")
	setAllow(t, p2, "/")
	assertGrades(t, p2, "/etc/proj/.env", 1, 2)
	for _, pat := range p2.DenyOverridePatterns() {
		if strings.Contains(pat, "//") {
			t.Errorf("DenyOverridePatterns must not contain double slash: %q", pat)
		}
	}
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
	got := compileDeny([]string{"%LOCALAPPDATA%/Google/Chrome/User Data/**"})
	if runtime.GOOS == "windows" {
		if len(got) == 0 {
			t.Errorf("windows: compiled %v, want >=1 entry (双形态可能 2 条)", got)
		}
		return
	}
	if len(got) != 0 {
		t.Errorf("non-windows: dangling %%LOCALAPPDATA%% entry must be skipped, got %v", got)
	}
	// 端到端：任意位置的 Google/Chrome/User Data 路径不得被 deny
	p := newTestPolicy(t, "")
	if p.DenyHit("/home/someone/Google/Chrome/User Data/Default/Cookies") {
		t.Error("dangling windows pattern must not deny arbitrary unix path")
	}
}

// TestCompileDenyDualForm：纯字面条目输出双形态（canonical + 原始展开形）——
// 模式自身是符号链接时（/var/run/docker.sock → 厂商 socket 形态）两形态是
// 不同的路径串，沙箱按实际传入路径匹配，双形态都须在名单内（实测 2026-09-05）。
// glob 条目仅输出 canonical 单形态。
func TestCompileDenyDualForm(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "real")
	mkdir(t, real)
	link := filepath.Join(base, "alias")
	if err := os.Symlink(real, link); err != nil {
		t.Skip("symlink unavailable:", err)
	}
	litPat := filepath.ToSlash(link) + "/x.txt"
	globPat := filepath.ToSlash(link) + "/**"
	got := compileDeny([]string{litPat, globPat})
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
	// glob 条目单形态（仅 canonical）
	wantGlob := canonicalPattern(mustExpand(t, globPat))
	globCount := 0
	for _, g := range got {
		if strings.Contains(g, "**") {
			globCount++
			if g != wantGlob {
				t.Fatalf("glob entry must stay single canonical form: got %q want %q", g, wantGlob)
			}
		}
	}
	if globCount != 1 {
		t.Fatalf("glob entry count = %d, want 1: %v", globCount, got)
	}
}

// TestGrantTemp：临时 grant（canonical 前缀、幂等、跨 session 失效、deny 校验）。
func TestGrantTemp(t *testing.T) {
	base := mkBase(t)
	ext := filepath.Join(base, "ext")
	mkdir(t, ext)
	p := newTestPolicy(t, "")

	assertGradesSid(t, p, "s1", ext+"/a.txt", 1, 3)
	p.Grant("s1", ext)
	p.Grant("s1", ext) // 幂等
	assertGradesSid(t, p, "s1", ext+"/a.txt", 1, 2)
	// 跨 session 失效
	assertGradesSid(t, p, "s2", ext+"/a.txt", 1, 3)

	// DenyHit：extraDeny 命中 / 界外失配
	setDeny(t, p, base+"/secrets/**")
	if !p.DenyHit(base + "/secrets/x") {
		t.Error("DenyHit should hit extraDeny")
	}
	if p.DenyHit(ext + "/x") {
		t.Error("DenyHit should miss outside deny")
	}
	// grant 与 deny 重叠时 deny 优先（Decide 先查 deny）
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
	// 经 link 访问 outside 下的文件 → canonical 后落在 outside（非白名单）→ 1/3
	assertGrades(t, p, link+"/f.txt", 1, 3)
	// link 自身的 canonical 身份 = outside（EvalSymlinks 成功）→ 同样 1/3：
	// 写 link 即写 outside，权限随真实目标
	assertGrades(t, p, link, 1, 3)
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

// TestDecideMissingTopLevelConsistency：顶层组件不存在时 deny/白名单判定仍成立——
// 模式与路径共用同一 canonical，退化形态下两端不得失配（判定向量钉死修复语义）。
func TestDecideMissingTopLevelConsistency(t *testing.T) {
	vol := filepath.VolumeName(os.TempDir())
	missing := filepath.ToSlash(vol + string(filepath.Separator) + "aic-nonexistent-xyz")
	p := newTestPolicy(t, "")
	setDeny(t, p, missing+"/secrets/**")
	assertGrades(t, p, missing+"/secrets/key.pem", 0, 0)
	p.mu.Lock()
	p.extraWrite = canonicalList([]string{missing + "/work"})
	p.rebuildBaseRootsLocked()
	p.mu.Unlock()
	assertGrades(t, p, missing+"/work/f.txt", 1, 2)
}

// TestWriteRootsFor：沙箱白名单 = 基础 + 配置 + grant，canonical 去重，跨 session 隔离。
func TestWriteRootsFor(t *testing.T) {
	base := mkBase(t)
	ws := filepath.Join(base, "ws")
	mkdir(t, ws)
	p := newTestPolicy(t, ws)
	p.mu.Lock()
	p.extraWrite = canonicalList([]string{base + "/custom"})
	p.rebuildBaseRootsLocked()
	p.mu.Unlock()
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

	assertGrades(t, p, custom+"/x", 1, 3)
	saved := cfg.Global
	defer func() { cfg.Global = saved }()
	o := cfg.NewOptions()
	o.FsAllow = []string{custom}
	o.FsDeny = []string{secrets + "/**"}
	cfg.Global = o
	p.Reconcile()
	assertGrades(t, p, custom+"/x", 1, 2)
	assertGrades(t, p, secrets+"/x", 0, 0)
	// Reconcile 后 deny 表重建：cfg 清空即恢复默认表
	o2 := cfg.NewOptions()
	cfg.Global = o2
	p.Reconcile()
	assertGrades(t, p, secrets+"/x", 1, 3)
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
