// Package fsauth 是物理 host 统一文件权限模型（aic docs/todo.md v0.14.5 §2）：
//
//	Decide(canonicalPath, write) → 所需等级：
//	deny（按平台分表的初始名单 + cfg fs_deny_paths 叠加，预展开缓存）→ 0（显式禁用，不可审批绕过）
//	读（非 deny）→ 1
//	写：白名单（work_dir/临时区/会话区/缓存/公共区 + cfg fs_write_roots + 临时 grant）→ 2
//	写：其余 → 3（危险写，逐次审批；grant_apply --permanent 可入白名单）
//
// 初始 deny 名单按平台分表（deny_{darwin,linux,windows,other}.go：三平台相关路径不同，
// 分表消除跨平台变量展开串扰风险）；通用凭证条目在 deny_common.go 单源。
//
// 判定一律在 canonical 层（EvalSymlinks 展开最近存在祖先后拼接剩余路径）——
// OSVFS 直跟 symlink，词法判定会被 grant 目录内的 link -> ~/.ssh 绕过。
//
// 性能：Decide 热路径零 syscall——deny 与根基底在 New/SetWorkDir/Reconcile 时
// 预展开/预计算（rebuildLocked），判定时纯内存匹配；evalSymlinks 只发生在
// 路径入参的 canonical() 上。沙箱 bind 白名单（WriteRootsFor）另经存在性
// 探测（bind 源必须存在），Start 频率低不缓存。
//
// 同一 Policy 实例注入 vcore Env.Policy（fs 工具动态判定）与 exec_procs 沙箱
// write bind 白名单（WriteRootsFor）——fs 与 exec 共用同一份名单。cloud 为信任域
// 不接本包（无 deny，WriteRoots 2/3 分级经 proto.WriteGrade 单源）。
package fsauth

import (
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/veypi/aic-pod/cfg"
	"github.com/veypi/aic-pod/libs/proto"
)

// Policy 是 host 级文件权限单例（进程内一份，装配点注入各处）。
type Policy struct {
	mu sync.RWMutex

	workDir    string   // 工作区（cfg work_dir；空 = 无）
	sessionDir string   // 会话区根（$HOME/.aic/sessions）
	publicDir  string   // 公共区（$HOME/.aic）
	extraWrite []string // cfg fs_write_roots（canonical 前缀）
	deny       []string // 拒绝模式（平台初始表 + cfg fs_deny_paths 叠加，预展开：expandVars + canonicalPattern）
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
	w, d := cfg.FsRoots()
	p.extraWrite = canonicalList(w)
	p.deny = compileDeny(append(defaultDenyPaths(), d...))
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

// DenyHit 报告 canonical 路径是否命中 deny 名单（grant_apply 校验用：
// deny_paths 内拒绝申请）。
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

// Decide 返回 (read, write) 所需等级（canonical 判定；deny → 0/0）。
func (v *View) Decide(path string) (int, int) {
	return v.p.decide(v.sid, canonical(path))
}

func (p *Policy) decide(sid, cpath string) (int, int) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.denyHit(cpath) {
		return 0, 0
	}
	if proto.InWriteRoots(cpath, p.decideRootsLocked(sid)) {
		return 1, 2
	}
	return 1, 3
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

// denyHit 判定 canonical 路径命中预展开 deny 表。
func (p *Policy) denyHit(cpath string) bool {
	for _, pat := range p.deny {
		if matchPattern(pat, cpath) {
			return true
		}
	}
	return false
}

// compileDeny 预展开拒绝模式：expandVars（~ / $VAR / %VAR% / $UserConfigDir）
// + canonicalPattern（字面前缀段展开符号链接——macOS /var 是 /private/var
// 的 symlink，用户写 /var/tmp/x/** 与 canonical 路径 /private/var/tmp/... 必须
// 仍能匹配）。展开失败（未定义变量、无家目录）的条目整条跳过——初始名单已按平台
// 分表，此规则只防御用户 cfg 条目（写了本平台不存在的变量时宁缺毋滥）。
func compileDeny(pats []string) []string {
	out := make([]string, 0, len(pats))
	for _, pat := range pats {
		e, ok := expandVars(pat)
		if !ok {
			continue
		}
		out = append(out, canonicalPattern(e))
	}
	return out
}

// Canonical 导出 canonical（包外少量场景用：grant_apply 落盘幂等比较等）。
func Canonical(p string) string { return canonical(p) }

// canonical 展开符号链接到真实文件系统身份：EvalSymlinks 逐级向父目录回退
// （目标不存在时展开最近存在祖先，剩余路径原样拼接——写新文件场景）。
// 反斜杠归一为 /（匹配器统一斜杠语义）。
// 递归到文件系统根基（/ 或 C:\）时直接返回原路径：根基无可展开项，且
// TrimSuffix 根基分隔符得空串/盘符，继续递归会退化成相对路径（"./x" 或
// 盘符相对形态），使绝对路径判定脱离绝对口径。
func canonical(p string) string {
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
