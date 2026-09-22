// Package fsauth enforces the host's local filesystem policy as one ordered
// rule table (aic/docs/permission_rules.md): builtin deny rows first, then cfg
// fs_rules, then permanent fs_grants; the last matching row wins and unmatched
// paths fall back to fs_policy. Reads are open except deny rows; ro rows poke
// read-only holes into wider deny rows above them. Session-layer write roots
// (convenience roots, temp grants) take effect only when the table does not
// resolve to deny. Native process sandboxes derive their lists from the same
// table; each lookup resolves symlinks first.
package fsauth

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"

	"github.com/veypi/aic-pod/cfg"
	"github.com/veypi/aic-pod/libs/policy"
	"github.com/veypi/aic-pod/libs/proto"
)

// Policy 是 host 级文件权限单例（进程内一份，装配点注入各处）。
type Policy struct {
	mu sync.RWMutex

	workDir    string // 工作区（cfg work_dir；空 = 无）
	sessionDir string // 会话区根（$HOME/.aic/sessions）
	publicDir  string // 公共区（$HOME/.aic）
	openMode   bool   // fs_policy=open：写除 deny 行外全放（2 级）
	rules      []fsRule
	grants     map[string][]string

	// baseRoots/decideCaches 预计算（重建点 = New/SetWorkDir/Reconcile，锁内）：
	// Decide 热路径零 syscall 的前提。派生自上述字段 + 平台缓存候选，禁止绕过
	// rebuildBaseRootsLocked 直接赋值。
	baseRoots    []string // workDir + 系统临时目录 + publicDir（全 canonical）
	decideCaches []string // 工具链缓存目录候选（cacheRootDirs，无存在性探测）
}

// fsEffect 是规则行的判定效果（policy 包的字符串形态在包边转换）。
type fsEffect uint8

const (
	effNone fsEffect = iota // 未命中（走 fs_policy 兜底）
	effDeny                 // 读写双拒
	effRO                   // 读开放、写拒（开读洞）
	effRW                   // 读写
)

func fsEffectOf(effect string) fsEffect {
	switch effect {
	case policy.EffectDeny:
		return effDeny
	case policy.EffectRO:
		return effRO
	default:
		return effRW
	}
}

func (e fsEffect) String() string {
	switch e {
	case effDeny:
		return policy.EffectDeny
	case effRO:
		return policy.EffectRO
	case effRW:
		return policy.EffectRW
	}
	return ""
}

// fsRule 是一条编译后的规则行（工具层判定与沙箱名单派生共用同一编译产物）。
type fsRule struct {
	eff  fsEffect
	src  string   // builtin | cfg | grant
	raw  string   // 原始行（explain/快照用）
	pats []string // resolve 用全形态：canonical + 裸模式子树展开 + 双拼写
	root []string // rw 行且为裸模式：写根双形态（沙箱 bind 用）
	glob bool     // rw 行且为通配模式：pats 即沙箱写 glob
}

// New 创建 Policy：会话区/公共区首次调用创建（best-effort，失败不阻断——
// 白名单宁缺毋滥）；workDir 经 SetWorkDir 同步（cfg 缺省为空 = 无工作区）。
func New() *Policy {
	p := &Policy{grants: map[string][]string{}}
	if dir, err := cfg.PublicDir(); err == nil {
		p.publicDir = canonical(dir)
		p.sessionDir = filepath.Join(p.publicDir, "sessions")
		_ = os.MkdirAll(p.sessionDir, 0o700)
	}
	p.mu.Lock()
	p.rebuildLocked()
	p.mu.Unlock()
	return p
}

// rebuildLocked 全量重算派生状态：有序规则表（builtin deny 出厂初始表 +
// cfg fs_rules + permanent fs_grants，后命中者胜）+ 根基底预计算。
// New/Reconcile 的统一出口；锁内调用。
func (p *Policy) rebuildLocked() {
	a := cfg.AuthSnapshot()
	p.openMode = a.FsPolicy == cfg.PolicyOpen
	rules := make([]fsRule, 0, len(defaultDenyPaths())+len(a.FsRules)+len(a.FsGrants))
	for _, d := range defaultDenyPaths() {
		if r, ok := compileFSRule(policy.EffectDeny+":"+d, "builtin"); ok {
			rules = append(rules, r)
		}
	}
	for _, raw := range a.FsRules {
		if r, ok := compileFSRule(raw, "cfg"); ok {
			rules = append(rules, r)
		}
	}
	for _, raw := range a.FsGrants {
		if r, ok := compileFSRule(raw, "grant"); ok {
			rules = append(rules, r)
		}
	}
	p.rules = rules
	p.rebuildBaseRootsLocked()
}

// rebuildBaseRootsLocked 重算根基底：workDir 变更（SetWorkDir）与 cfg 变更
// （rebuildLocked）共用；锁内调用。便利根不入规则表——它们只在表判定非 deny
// 时授写（permission_rules.md §2），天然保持"便利不压 deny"。
func (p *Policy) rebuildBaseRootsLocked() {
	roots := []string{}
	if p.workDir != "" {
		roots = append(roots, dualForms(p.workDir)...)
	}
	if t := os.TempDir(); t != "" {
		roots = append(roots, dualForms(t)...)
	}
	roots = append(roots, dualList(tempRoots())...)
	if p.publicDir != "" {
		roots = append(roots, dualForms(p.publicDir)...)
	}
	p.baseRoots = dedupClean(roots)
	// Decide 侧缓存目录不做存在性探测：前缀匹配对不存在目录天然生效，白名单
	// 意图跟目录身份走（未装 rust 时写 ~/.cargo 也是白名单意图内的工具链缓存）。
	// bind 侧源必须存在，走 CacheRoots() 实时探测——两侧自此语义分离。
	p.decideCaches = dedupClean(dualList(cacheRootDirs())) // 去空+双拼写
}

// SetWorkDir 同步工作区（set_config / Reconfigure；空 = 清除）并重算根基底。
func (p *Policy) SetWorkDir(wd string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if wd != "" {
		p.workDir = canonical(wd)
	} else {
		p.workDir = ""
	}
	p.rebuildBaseRootsLocked()
}

// Reconcile 运行配置变更后重载（cfg.Global 已由调用方更新；重算规则表
// 与根基底预计算）。
func (p *Policy) Reconcile() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.rebuildLocked()
}

// Grant 临时授权：path（canonical 前缀）加入 sid 的可写名单——
// 重启失效、跨 session 失效（host 进程内存）；幂等。
// 注意：host 无 session 结束钩子，grants 不主动清理（量小，重启即清零）。
func (p *Policy) Grant(sid, path string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	path = canonical(path)
	for _, g := range p.grants[sid] {
		if g == path {
			return
		}
	}
	p.grants[sid] = append(p.grants[sid], path)
}

// OpenMode 报告 fs_policy 是否为 open（写除 deny 行全放）——沙箱 profile
// 生成用（darwin allow file-write* 打底 / bwrap 整机 rw，deny 行仍生效）。
func (p *Policy) OpenMode() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.openMode
}

// resolveLocked 顺序求值规则表：按拼接序逐条匹配，最后命中者胜；未命中返回 effNone。
func (p *Policy) resolveLocked(cpath string) fsEffect {
	eff := effNone
	for _, r := range p.rules {
		for _, pat := range r.pats {
			if matchPattern(pat, cpath) {
				eff = r.eff
				break
			}
		}
	}
	return eff
}

// RuleInfo 是规则表的只读快照行（M3 沙箱行序映射与 explain 干跑用）。
type RuleInfo struct {
	Effect   string   // deny | ro | rw
	Source   string   // builtin | cfg | grant
	Raw      string   // 原始规则行
	Patterns []string // 编译后的匹配形态
}

// Rules 返回有序规则表快照（拼接序 = 匹配序）。
func (p *Policy) Rules() []RuleInfo {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]RuleInfo, len(p.rules))
	for i, r := range p.rules {
		out[i] = RuleInfo{Effect: r.eff.String(), Source: r.src, Raw: r.raw, Patterns: append([]string{}, r.pats...)}
	}
	return out
}

// DenyPatterns 返回全部 deny 行的编译模式快照——exec 沙箱拒绝规则
// （§5.10 deny 隔离）与 fs 判定同源派生；cfg 变更经 Reconcile 重算后，
// 本次调用的 Start 即取到新名单（沙箱每次 Start 构造 profile）。
// 注意（M3 前）：沙箱按 deny 行全量落隔离，不表达 deny 行之内的 ro/rw 洞——
// 洞内目标在沙箱内仍被拒（fail-closed），仅工具层放行（permission_rules.md §5）。
func (p *Policy) DenyPatterns() []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	var out []string
	for _, r := range p.rules {
		if r.eff == effDeny {
			out = append(out, r.pats...)
		}
	}
	return dedupClean(out)
}

// DenyHit 报告路径的表判定终局是否为 deny（temp grant 校验用：
// deny 终局拒绝申请——session 层不得放宽表判定的 deny，§2 硬底线）。
func (p *Policy) DenyHit(path string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.resolveLocked(canonical(path)) == effDeny
}

// LastDenyRow 报告路径最后命中的 deny 行序号（1 起，拼接序）与原始行——
// grant --permanent 的覆盖提示用（与 DenyHit 配合：仅当终局为 deny 时展示）。
func (p *Policy) LastDenyRow(path string) (int, string, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	cp := canonical(path)
	idx := -1
	for i, r := range p.rules {
		if r.eff != effDeny {
			continue
		}
		for _, pat := range r.pats {
			if matchPattern(pat, cp) {
				idx = i
				break
			}
		}
	}
	if idx < 0 {
		return 0, "", false
	}
	return idx + 1, p.rules[idx].raw, true
}

// WriteRootsFor 返回 sid 的沙箱 write bind 白名单（便利根 + rw 行裸模式根 +
// 临时 grant，canonical + 去重）——exec_procs workspace-write 的可写根。
func (p *Policy) WriteRootsFor(sid string) []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return dedupClean(p.bindRootsLocked(sid))
}

// View 返回会话绑定视图（临时 grant 与会话区按 sid 生效）；注入 vcore Env.Policy。
// sid 为空 = 无会话上下文（仅基础白名单）。
func (p *Policy) View(sid string) *View { return &View{p: p, sid: sid} }

// View 实现 vcore.PathPolicy（Decide 不含 sid 参数——sid 在视图绑定时固化）。
type View struct {
	p   *Policy
	sid string
}

// Decide 返回 (read, write) 所需等级：deny 行 → 0/0（读写双拒，session 层不可绕）；
// rw 行或可写根（便利根/会话区/temp grant）→ 1/2；ro 行或未命中 → 1/0
// （open 姿态未命中 → 1/2）。
func (v *View) Decide(path string) (int, int) {
	return v.p.decide(v.sid, canonical(path))
}

// DecideNoFollow 是 unlink/rename 语义的判定入口：末段符号链接不跟随（删/挪的
// 是链接本身，不会触达目标），父链展开、规则表判定不变。rm/mv 类操作必须
// 走此入口——否则可写根内的外向链接会被无关目标路径的策略拒掉（2026-09-22 实测：
// .venv/bin/python -> /opt/homebrew/... 致 fs rm 整个 venv 目录被阻）。
func (v *View) DecideNoFollow(path string) (int, int) {
	return v.p.decide(v.sid, canonicalNoFollow(path))
}

func (p *Policy) decide(sid, cpath string) (int, int) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	eff := p.resolveLocked(cpath)
	if eff == effDeny {
		return 0, 0
	}
	if eff == effRW {
		return 1, 2
	}
	// eff ∈ {ro, none}：会话写根（便利根/会话区/temp grant）仅在表判定非 deny
	// 时生效（§3）——ro 不挡会话写根（temp grant 可把 ro 目标提升为可写）。
	if proto.InWriteRoots(cpath, p.decideRootsLocked(sid)) {
		return 1, 2
	}
	// open 姿态写全放，ro 行是其唯一约束（§8.5）。
	if eff == effNone && p.openMode {
		return 1, 2
	}
	return 1, 0
}

// decideRootsLocked 返回 sid 的判定根集（纯内存拼接，零 syscall）：
// 预计算基底 + 缓存候选 + 会话区 + 临时 grant。
func (p *Policy) decideRootsLocked(sid string) []string {
	roots := make([]string, 0, len(p.baseRoots)+len(p.decideCaches)+2)
	roots = append(roots, p.baseRoots...)
	roots = append(roots, p.decideCaches...)
	if sid != "" && p.sessionDir != "" {
		roots = append(roots, filepath.Join(p.sessionDir, sid))
	}
	roots = append(roots, p.grants[sid]...)
	return roots
}

// bindRootsLocked 返回 sid 的沙箱 bind 白名单根集：便利根 + rw 行裸模式根
// + 会话区 + 临时 grant；缓存目录经存在性探测（bwrap/seatbelt bind 要求源存在；
// Start 频率低，实时探测不缓存）。WriteRootsFor 专用。
func (p *Policy) bindRootsLocked(sid string) []string {
	roots := make([]string, 0, len(p.baseRoots)+8)
	roots = append(roots, p.baseRoots...)
	roots = append(roots, CacheRoots()...)
	if sid != "" && p.sessionDir != "" {
		roots = append(roots, filepath.Join(p.sessionDir, sid))
	}
	roots = append(roots, p.grants[sid]...)
	for _, r := range p.rules {
		if r.eff == effRW && !r.glob {
			roots = append(roots, r.root...)
		}
	}
	return roots
}

// WritePatternsFor 返回 rw 行通配模式（与写根同源派生，沙箱写 glob 放行用）。
func (p *Policy) WritePatternsFor(sid string) []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	var out []string
	for _, r := range p.rules {
		if r.eff == effRW && r.glob {
			out = append(out, r.pats...)
		}
	}
	return dedupClean(out)
}

func (p *Policy) DropSession(sid string) { p.mu.Lock(); defer p.mu.Unlock(); delete(p.grants, sid) }

// joinPattern 拼接「根 + 子模式」（根为 "/" 时不产生 "//"——matchPattern 按 / 分段，
// 双斜杠会引入空段使模式整体失配）。
func joinPattern(root, sub string) string {
	if root == "" {
		// 空 root 绝不允许拼出 "/**"（匹配全部路径）：返回空串，调用方丢弃。
		// 2026-09-22：GOCACHE/XDG_CACHE_HOME 未设置 → 空缓存根曾把读放行名单
		// 退化成全匹配（读锁失效）；该防护对写根同样成立。
		return ""
	}
	if root == "/" {
		return "/" + sub
	}
	return strings.TrimSuffix(root, "/") + "/" + sub
}

// compileFSRule 编译一条规则行：policy.ParseFSRule（效果前缀 + 全域禁写校验）
// + expandVars + canonicalPattern（字面前缀段符号链接展开）+ 裸模式子树展开
// + 双形态（模式自身是符号链接时两形态是不同的路径串，须都在名单内才能双命中：
// lstat 查字面形、open/connect 查解析形）。展开失败（未定义变量/无家目录）或
// 非法行整条跳过（宁缺毋滥——cfg 校验已在加载/保存路径点名报错）。
func compileFSRule(raw, src string) (fsRule, bool) {
	eff, pat, err := policy.ParseFSRule(raw)
	if err != nil {
		return fsRule{}, false
	}
	e, ok := expandVars(pat)
	if !ok || e == "" {
		return fsRule{}, false
	}
	r := fsRule{eff: fsEffectOf(eff), src: src, raw: raw}
	cp := canonicalPattern(e)
	r.pats = append(r.pats, cp)
	if strings.ContainsAny(e, "*?") {
		r.glob = true
		if cp != e {
			r.pats = append(r.pats, e)
		}
	} else {
		r.pats = append(r.pats, joinPattern(cp, "**"))
		if lit := filepath.ToSlash(e); lit != cp {
			r.pats = append(r.pats, lit, joinPattern(lit, "**"))
		}
		if r.eff == effRW {
			r.root = dualForms(e)
		}
	}
	r.pats = dedupClean(r.pats)
	return r, true
}

// Canonical 导出 canonical（包外少量场景用：grant fs 落盘幂等比较等）。
func Canonical(p string) string { return canonical(p) }

// bareDriveRe 匹配裸盘符形态（"C:"）。
var bareDriveRe = regexp.MustCompile(`^[A-Za-z]:$`)

// isBareDrive 报告 p 是否为 windows 裸盘符（非 windows 恒 false——posix 上
// "C:" 是普通相对路径名，不做特殊处理）。
func isBareDrive(p string) bool {
	return runtime.GOOS == "windows" && bareDriveRe.MatchString(p)
}

// canonical 展开符号链接到真实文件系统身份：EvalSymlinks 逐级向父目录回退
// （目标不存在时展开最近存在祖先，剩余路径原样拼接——写新文件场景）。
// 反斜杠归一为 /（匹配器统一斜杠语义）。
// 递归到文件系统根基（/ 或 C:\）时直接返回原路径：根基无可展开项，且
// TrimSuffix 根基分隔符得空串/盘符，继续递归会退化成相对路径（"./x" 或
// 盘符相对形态），使绝对路径判定脱离绝对口径。
// windows 裸盘符（"C:"）先补为盘根（"C:\"）再展开：EvalSymlinks 对裸盘符
// 是盘符当前目录语义，权限判定必须按盘根口径（与 ProtectRoots 的 "C:" 可比）。
func canonical(p string) string {
	if isBareDrive(p) {
		p += `\`
	}
	p = filepath.Clean(p)
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return filepath.ToSlash(r)
	}
	dir, base := filepath.Split(p)
	if dir == "" {
		return filepath.ToSlash(p)
	}
	parent := strings.TrimSuffix(dir, string(filepath.Separator))
	if parent == filepath.VolumeName(dir) {
		return filepath.ToSlash(p)
	}
	return canonical(parent) + "/" + base
}

// CanonicalNoFollow 导出 canonicalNoFollow（rm/mv 判定与 grant 归一用）。
func CanonicalNoFollow(p string) string { return canonicalNoFollow(p) }

// canonicalNoFollow 展开父目录符号链接、保留末段字面形（unlink/rename 语义：
// 系统调用作用于链接本身，判定必须同口径）。父链复用 canonical（含最近存在
// 祖先回退）；末段为根/退化形态时退回跟随版。
func canonicalNoFollow(p string) string {
	if isBareDrive(p) {
		p += `\`
	}
	p = filepath.Clean(p)
	parent := filepath.Dir(p)
	base := filepath.Base(p)
	if base == "." || base == string(filepath.Separator) || parent == p {
		return canonical(p)
	}
	return filepath.ToSlash(filepath.Join(canonical(parent), base))
}

func dedupClean(roots []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(roots))
	for _, r := range roots {
		if r == "" || seen[r] {
			continue
		}
		seen[r] = true
		out = append(out, r)
	}
	return out
}

// appendEnvDirs 只追加非空环境变量目录：空值经 joinPattern 会变成 "/**"
// （匹配全部路径），写白名单会因此放大到全盘——2026-09-22 修复：
// GOCACHE/XDG_CACHE_HOME 未设置时空根曾使读放行名单退化为全匹配。
func appendEnvDirs(dirs []string, names ...string) []string {
	for _, name := range names {
		if v := os.Getenv(name); v != "" {
			dirs = append(dirs, v)
		}
	}
	return dirs
}

// dualForms 返回路径的 canonical 形与原始字面形（去尾斜杠；相同则只给一个）。
// 沙箱（seatbelt）按系统调用实际传入的路径串匹配规则：macOS 的 /tmp、/etc、
// $TMPDIR(/var/folders/…) 都是 symlink 前缀，只留 canonical 形会让字面拼写的
// 访问（含路径解析的 metadata 读）被拒——与规则编译的「双形态」对称
// （2026-09-22）。
func dualForms(p string) []string {
	p = strings.TrimSpace(p)
	if p == "" {
		return nil
	}
	for len(p) > 1 && strings.HasSuffix(p, "/") {
		p = strings.TrimSuffix(p, "/")
	}
	c := canonical(p)
	if c == "" {
		c = filepath.Clean(p)
	}
	if c == p {
		return []string{c}
	}
	return []string{c, p}
}

// dualList 对一组路径逐个展开 dualForms（去重交给 dedupClean）。
func dualList(paths []string) []string {
	out := make([]string, 0, len(paths)*2)
	for _, p := range paths {
		out = append(out, dualForms(p)...)
	}
	return out
}

// canonicalPattern 对 glob 模式的字面前缀段（首个含通配符段之前）做
// canonical 展开：/var/tmp/x/** → /private/var/tmp/x/**（macOS /var symlink）。
// 纯字面模式（无通配符）：整串展开；** 开头的模式无前缀可展开（原样返回）。
func canonicalPattern(pat string) string {
	pat = filepath.ToSlash(pat)
	segs := strings.Split(pat, "/")
	lit := 0
	for _, s := range segs {
		// 与 matchOne 实现口径一致：只认 * ?（[ 按字面匹配，检测集不得超实现集，
		// 否则含 [ 的模式会在此误断字面前缀、预展开错位）
		if strings.ContainsAny(s, "*?") {
			break
		}
		lit++
	}
	if lit == 0 {
		return pat
	}
	prefix := canonical(strings.Join(segs[:lit], "/"))
	if lit == len(segs) {
		return prefix // 纯字面模式（无通配符）：整串展开
	}
	return prefix + "/" + strings.Join(segs[lit:], "/")
}

// expandVars 展开模式中的 ~（~ 与 ~/ 前缀）与环境变量：$VAR/${VAR}（unix）、
// %VAR%（windows 形态，跨平台都展开——用户 cfg 条目可能引用任一形态）、特殊记号
// $UserConfigDir（Go 的 os.UserConfigDir，无对应环境变量）。
// 任一变量的展开失败（未定义变量、无家目录）使整条模式失配（ok=false）——
// 初始名单已按平台分表，此规则只防御用户 cfg 条目（写了本平台不存在的变量时宁缺毋滥）。
func expandVars(s string) (string, bool) {
	home, herr := os.UserHomeDir()
	if s == "~" {
		if herr != nil {
			return "", false
		}
		s = home
	} else if strings.HasPrefix(s, "~/") {
		if herr != nil {
			return "", false
		}
		s = home + s[1:]
	}
	ok := true
	expand := func(name string) string {
		if name == "UserConfigDir" {
			d, err := os.UserConfigDir()
			if err != nil {
				ok = false
				return ""
			}
			return d
		}
		v, found := os.LookupEnv(name)
		if !found {
			ok = false
			return ""
		}
		return v
	}
	s = os.Expand(s, expand)
	for {
		i := strings.IndexByte(s, '%')
		if i < 0 {
			break
		}
		j := strings.IndexByte(s[i+1:], '%')
		if j < 0 {
			break
		}
		name := s[i+1 : i+1+j]
		s = s[:i] + expand(name) + s[i+j+2:]
	}
	return filepath.ToSlash(s), ok
}
