// Package fsauth enforces the host's local filesystem policy. Explicit denies
// take precedence over all grants. Read-only allow rules grant reads; writable
// allow rules grant reads and writes. A closed policy rejects unmatched paths.
// Cloud approval never modifies this policy. Native process sandboxes consume
// the same read/write scopes and denies; each lookup resolves symlinks first.
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
	openMode   bool   // fs_policy=open：写除 deny 名单外全放（2 级）
	readRoots  []string
	readGlobs  []string
	extraWrite []string // cfg fs_allow 裸路径条目（canonical 前缀）
	allowGlobs []string // cfg fs_allow 通配条目（canonicalPattern 展开：精确授权 + 压 deny）
	deny       []string // 拒绝模式（平台初始表 + cfg fs_deny 叠加，预展开：expandVars + canonicalPattern）
	grants     map[string][]string

	// baseRoots/decideCaches 预计算（重建点 = New/SetWorkDir/Reconcile，锁内）：
	// Decide 热路径零 syscall 的前提。派生自上述字段 + 平台缓存候选，禁止绕过
	// rebuildBaseRootsLocked 直接赋值。
	baseRoots    []string // workDir + 系统临时目录 + publicDir + extraWrite（全 canonical）
	decideCaches []string // 工具链缓存目录候选（cacheRootDirs，无存在性探测）
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

// rebuildLocked 全量重算派生状态（cfg 白名单/deny + 根基底预计算）。
// New/Reconcile 的统一出口；锁内调用。
func (p *Policy) rebuildLocked() {
	a := cfg.AuthSnapshot()
	p.openMode = a.FsPolicy == cfg.PolicyOpen
	var rw, ro []string
	for _, raw := range a.FsAllow {
		item, err := policy.ParseFSAllow(raw)
		if err != nil {
			continue
		}
		if item.ReadOnly {
			ro = append(ro, item.Path)
		} else {
			rw = append(rw, item.Path)
		}
	}
	p.extraWrite, p.allowGlobs = splitAllow(rw)
	p.readRoots, p.readGlobs = splitAllow(ro)
	p.readGlobs = append(p.readGlobs, SystemCAReadPatterns()...)
	p.readRoots = append(p.readRoots, RuntimeReadRoots()...)
	p.deny = compileDeny(append(defaultDenyPaths(), a.FsDeny...))
	p.rebuildBaseRootsLocked()
}

// rebuildBaseRootsLocked 重算根基底：workDir 变更（SetWorkDir）与 cfg 变更
// （rebuildLocked）共用；锁内调用。
func (p *Policy) rebuildBaseRootsLocked() {
	roots := []string{}
	if p.workDir != "" {
		roots = append(roots, p.workDir)
	}
	if t := os.TempDir(); t != "" {
		roots = append(roots, canonical(t))
	}
	if p.publicDir != "" {
		roots = append(roots, p.publicDir)
	}
	roots = append(roots, p.extraWrite...)
	p.baseRoots = roots
	// Decide 侧缓存目录不做存在性探测：前缀匹配对不存在目录天然生效，白名单
	// 意图跟目录身份走（未装 rust 时写 ~/.cargo 也是白名单意图内的工具链缓存）。
	// bind 侧源必须存在，走 CacheRoots() 实时探测——两侧自此语义分离。
	p.decideCaches = cacheRootDirs()
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

// Reconcile 运行配置变更后重载（cfg.Global 已由调用方更新；重算白名单/deny
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

// OpenMode 报告 fs_policy 是否为 open（写除 deny 全放）——沙箱 profile
// 生成用（darwin allow file-write* 打底 / bwrap 整机 rw，deny 覆盖仍生效）。
func (p *Policy) OpenMode() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.openMode
}

// DenyPatterns 返回预展开的 deny 模式快照（compileDeny 产物：变量展开 +
// canonical 字面前缀）——exec 沙箱拒绝规则（§5.10 deny 隔离）与 fs 判定共用
// 同一份名单：cfg fs_deny / set_config 变更经 Reconcile 重算后，
// 本次调用的 Start 即取到新名单（沙箱每次 Start 构造 profile）。
func (p *Policy) DenyPatterns() []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]string, len(p.deny))
	copy(out, p.deny)
	return out
}

// DenyHit rejects grant targets covered by a local deny. No allow overrides it.
func (p *Policy) DenyHit(path string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.denyHit(canonical(path))
}

// WriteRootsFor 返回 sid 的沙箱 write bind 白名单（基础白名单 + 配置 +
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

// Decide 返回 (read, write) 所需等级（canonical 判定；deny → 0/0，
// 显式 fs_allow 条目命中 → 回落正常分级并豁免 deny）。
func (v *View) Decide(path string) (int, int) {
	return v.p.decide(v.sid, canonical(path))
}

func (p *Policy) decide(sid, cpath string) (int, int) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.denyHit(cpath) {
		return 0, 0
	}
	if p.openMode || p.allowHitLocked(cpath) || proto.InWriteRoots(cpath, p.decideRootsLocked(sid)) {
		return 1, 2
	}
	if proto.InWriteRoots(cpath, p.readRoots) {
		return 1, 0
	}
	for _, pat := range p.readGlobs {
		if matchPattern(pat, cpath) {
			return 1, 0
		}
	}
	return 0, 0
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

// bindRootsLocked 返回 sid 的沙箱 bind 白名单根集：缓存目录经存在性探测
// （bwrap/seatbelt bind 要求源存在；Start 频率低，实时探测不缓存——目录首次
// 创建后无需重建即可进 bind）。WriteRootsFor 专用，与 Decide 判定根集的
// 差异仅在缓存目录的存在性过滤。
func (p *Policy) bindRootsLocked(sid string) []string {
	roots := make([]string, 0, len(p.baseRoots)+8)
	roots = append(roots, p.baseRoots...)
	roots = append(roots, CacheRoots()...)
	if sid != "" && p.sessionDir != "" {
		roots = append(roots, filepath.Join(p.sessionDir, sid))
	}
	roots = append(roots, p.grants[sid]...)
	return roots
}

// denyHit 判定 canonical 路径命中预展开 deny 表（原始表，不含例外）。
func (p *Policy) denyHit(cpath string) bool {
	for _, pat := range p.deny {
		if matchPattern(pat, cpath) {
			return true
		}
	}
	return false
}

// allowHitLocked 判定显式 allow 条目命中（cfg fs_allow）：裸路径条目覆盖其子树
// （root 自身 + 全部后代），通配条目按 glob 精确匹配。命中 → 写 2 且豁免 deny
// 的读写双拒（allow 覆盖 deny）。内建便利根与临时 grant 不在本判定内——它们
// 是写便利不是信任声明（公共区的 browser cookie 库、缓存/工作区里的 .env 等
// 仍受 deny 保护）；要开洞必须显式写 fs_allow。判定在 canonical 路径上进行，
// symlink 跳转出 allow 条目不豁免（与 deny 同口径）。
func (p *Policy) allowHitLocked(cpath string) bool {
	for _, root := range p.extraWrite {
		if matchPattern(joinPattern(root, "**"), cpath) {
			return true
		}
	}
	for _, pat := range p.allowGlobs {
		if matchPattern(pat, cpath) {
			return true
		}
	}
	return false
}

// joinPattern 拼接「根 + 子模式」（根为 "/" 时不产生 "//"——matchPattern 按 / 分段，
// 双斜杠会引入空段使模式整体失配）。
func joinPattern(root, sub string) string {
	if root == "/" {
		return "/" + sub
	}
	return strings.TrimSuffix(root, "/") + "/" + sub
}

// ReadPatternsFor is the same read scope used by fs and process confinement.
func (p *Policy) ReadPatternsFor(sid string) []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := append([]string{}, p.readGlobs...)
	roots := append(append([]string{}, p.readRoots...), p.decideRootsLocked(sid)...)
	for _, r := range roots {
		out = append(out, joinPattern(r, "**"))
	}
	out = append(out, p.allowGlobs...)
	return out
}

// WritePatternsFor adds glob grants to the same write roots used by fs.
func (p *Policy) WritePatternsFor(sid string) []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return append([]string{}, p.allowGlobs...)
}
func (p *Policy) DropSession(sid string) { p.mu.Lock(); defer p.mu.Unlock(); delete(p.grants, sid) }

// splitAllow 拆分 cfg fs_allow 条目：裸路径（无通配）→ 写白名单根（canonical
// 前缀，覆盖子树）；带通配条目 → 精确 glob（canonicalPattern 只展开字面前缀，
// 与 compileDeny 同口径）。两类条目均不能覆盖 deny；展开失败
// （未定义变量/无家目录）的条目整条跳过（宁缺毋滥，同 compileDeny）。
func splitAllow(entries []string) (roots, globs []string) {
	for _, raw := range entries {
		e, ok := expandVars(strings.TrimSpace(raw))
		if !ok || e == "" {
			continue
		}
		if strings.ContainsAny(e, "*?") {
			globs = append(globs, canonicalPattern(e))
			continue
		}
		roots = append(roots, canonical(e))
	}
	return roots, globs
}

// compileDeny 预展开拒绝模式：expandVars（~ / $VAR / %VAR% / $UserConfigDir）
// + canonicalPattern（字面前缀段展开符号链接——macOS /var 是 /private/var
// 的 symlink，用户写 /var/tmp/x/** 与 canonical 路径 /private/var/tmp/... 必须
// 仍能匹配）。展开失败（未定义变量、无家目录）的条目整条跳过——初始名单已按平台
// 分表，此规则只防御用户 cfg 条目（写了本平台不存在的变量时宁缺毋滥）。
//
// 双形态（2026-09-05）：纯字面条目同时输出 canonical 形与原始展开形——沙箱按系统调用
// 实际传入的路径串匹配，模式自身是符号链接时（/var/run/docker.sock → 厂商 socket）
// 两形态是不同的路径串，须都在名单内才能双命中（lstat 查字面形、open/connect 查解析形）。
// glob 条目仅输出 canonical 形（通配形态天然覆盖两路）。
func compileDeny(pats []string) []string {
	out := make([]string, 0, len(pats)*2)
	seen := map[string]bool{}
	add := func(s string) {
		if s == "" || seen[s] {
			return
		}
		seen[s] = true
		out = append(out, s)
	}
	for _, pat := range pats {
		e, ok := expandVars(pat)
		if !ok {
			continue
		}
		add(canonicalPattern(e))
		if !strings.ContainsAny(e, "*?") {
			add(joinPattern(canonicalPattern(e), "**"))
			add(filepath.ToSlash(e))
		}
	}
	return out
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

func canonicalList(paths []string) []string {
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if e, ok := expandVars(p); ok {
			out = append(out, canonical(e))
		}
	}
	return out
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

// canonicalPattern 对 glob 模式的字面前缀段（首个含通配符段之前）做
// canonical 展开：/var/tmp/x/** → /private/var/tmp/x/**（macOS /var symlink）。
// 纯字面模式整串展开；** 开头的模式无前缀可展开（原样返回）。
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
		s = s[:i] + expand(name) + s[i+1+j+1:]
	}
	return filepath.ToSlash(s), ok
}

func (p *Policy) ResetTemporary() { p.mu.Lock(); defer p.mu.Unlock(); p.grants = map[string][]string{} }
