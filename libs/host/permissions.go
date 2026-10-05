package host

// permissionState：host 唯一运行权限状态（四仓精简 §4，2026-10-06）。
// 一份已编译 FS/net/SSH/cmd 基表 + 一个 map[sessionID]sessionGrants
// （按字段保存四域临时授权），一把锁负责一致快照。FS、原生命令、MCP、
// SSH、grant status 全部从它读取；匹配算法由 vbox 提供。
//
// 基表生命周期：
//   - 启动：由 cfg.Global 编译（newPermissionState）；配置无效 → invalid
//     非 nil，一切设备工具 fail-closed（修复本地设置并重启后恢复），
//     客户端照常连接。
//   - permanent grant：受串行保护的更新（persistGrant）——构造候选配置 →
//     完整编译校验（compileAuth）→ 成功保存 → 原子发布（publish）；
//     任一步失败均不发布部分规则。
//   - 临时 grant：只更新 grants 字段（会话隔离，重启失效）。
//
// cfg.Global 只是启动参数，不再是第二份并发运行权限状态；运行中修改
// cfg.Global 不影响本状态（设置继续重启生效）。
import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/veypi/aic-pod/cfg"
	"github.com/veypi/vbox"
)

// sessionGrants 按域保存一个会话的临时授权。
type sessionGrants struct {
	fs  []string
	net []vbox.Entry
	ssh []vbox.Entry
	cmd []string
}

// compiledAuth 是一次完整编译校验后的基表（publish 的输入）。
type compiledAuth struct {
	fsOpen    bool
	fsRules   []vbox.Rule // cfg 规则 + 内建 deny（首命中）
	netOpen   bool
	netRules  []vbox.NetRule
	sshOpen   bool
	sshRules  []vbox.NetRule
	execMode  string
	execRules []string
}

type permissionState struct {
	mu                            sync.RWMutex
	workDir, sessionDir, stateDir string
	baseRoots, decideCaches       []string
	base                          compiledAuth
	grants                        map[string]*sessionGrants
	invalid                       error // 启动配置无效：工具 fail-closed
}

// netBuiltin 是 net 域内建放行（本机回环；ssh 域无内建条目）。
var netBuiltin = []vbox.NetRule{{HostPort: "localhost:*", Allow: true}}

// compileAuth 完整编译校验一份授权配置（纯函数，不触碰运行状态）。
func compileAuth(a cfg.AuthCfg) (*compiledAuth, error) {
	if err := validateAuthCfg(a); err != nil {
		return nil, err
	}
	rules, err := vbox.CompileFSRules(a.FsRules, vbox.ClassCfg)
	if err != nil {
		return nil, err
	}
	defaults := defaultDenyPaths()
	for i := range defaults {
		defaults[i] = "deny:" + defaults[i]
	}
	builtin, err := vbox.CompileFSRules(defaults, vbox.ClassBuiltin)
	if err != nil {
		return nil, err
	}
	net, err := compileNetRules(a.NetRules)
	if err != nil {
		return nil, err
	}
	ssh, err := compileNetRules(a.SshRules)
	if err != nil {
		return nil, err
	}
	return &compiledAuth{
		fsOpen:    a.FsPolicy == cfg.PolicyOpen,
		fsRules:   append(rules, builtin...),
		netOpen:   a.NetPolicy == cfg.PolicyOpen,
		netRules:  net,
		sshOpen:   a.SshPolicy == cfg.PolicyOpen,
		sshRules:  ssh,
		execMode:  a.ExecPolicy,
		execRules: append([]string(nil), a.ExecRules...),
	}, nil
}

// validateAuthCfg 与 Options.ValidateAuth 同口径（AuthCfg 已归一 policy 值）。
func validateAuthCfg(a cfg.AuthCfg) error {
	if err := vbox.ValidateFSRules(a.FsRules); err != nil {
		return err
	}
	if err := cfg.ValidateExecRules(a.ExecRules); err != nil {
		return err
	}
	for _, list := range [][]string{a.NetRules, a.SshRules} {
		if err := vbox.ValidateTargetRules(list); err != nil {
			return err
		}
	}
	return nil
}

func compileNetRules(rows []string) ([]vbox.NetRule, error) {
	var compiled []vbox.NetRule
	for _, raw := range rows {
		allow, e, err := vbox.ParseTargetRule(raw)
		if err != nil {
			return nil, err
		}
		compiled = append(compiled, vbox.NetRule{HostPort: e.String(), Allow: allow})
	}
	return compiled, nil
}

// newPermissionState 构建启动权限状态。StateDir 失败是环境错误（返回
// error，同旧 fsauth.New 的 initErr 语义）；授权配置无效不报错——状态带
// invalid，工具 fail-closed（修复并重启后恢复），客户端照常运行。
// 校验按原始值（o.ValidateAuth）：非法 policy 取值等不能被读点归一化
// 吞掉；编译输入 AuthFrom 才做安全侧归一。
func newPermissionState(workDir string, o *cfg.Options) (*permissionState, error) {
	p := &permissionState{grants: map[string]*sessionGrants{}}
	dir, err := cfg.StateDir()
	if err != nil {
		return nil, err
	}
	p.stateDir = vbox.Canonical(dir)
	p.sessionDir = p.stateDir + "/sessions"
	p.setWorkDir(workDir)
	if err := o.ValidateAuth(); err != nil {
		p.invalid = fmt.Errorf("invalid authorization configuration: %w", err)
		return p, nil
	}
	base, err := compileAuth(cfg.AuthFrom(o))
	if err != nil {
		p.invalid = fmt.Errorf("invalid authorization configuration: %w", err)
	} else {
		p.base = *base
	}
	return p, nil
}

func (p *permissionState) setWorkDir(wd string) {
	p.workDir = ""
	if wd != "" {
		p.workDir = vbox.Canonical(wd)
	}
	p.rebuildRoots()
}

func (p *permissionState) rebuildRoots() {
	roots := append([]string{p.workDir, os.TempDir()}, tempRoots()...)
	p.baseRoots = vbox.DedupClean(vbox.DualList(roots))
	p.decideCaches = vbox.DedupClean(vbox.DualList(cacheRootDirs()))
}

// publish 原子发布新基表（permanent grant 保存成功后调用；输入已完整编译）。
func (p *permissionState) publish(base *compiledAuth) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.base = *base
	p.invalid = nil
}

// err 返回启动配置无效错误（nil = 配置有效）。
func (p *permissionState) err() error {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.invalid
}

func (p *permissionState) session(sid string) *sessionGrants {
	g := p.grants[sid]
	if g == nil {
		g = &sessionGrants{}
		p.grants[sid] = g
	}
	return g
}

// ---- 临时授权（会话隔离；temp 行插表头、首命中压一切） ----

func (p *permissionState) grantFS(sid, target string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	g := p.session(sid)
	target = vbox.Canonical(target)
	g.fs = vbox.DedupClean(append([]string{target}, g.fs...))
}

func (p *permissionState) grantNet(sid string, e vbox.Entry, ssh bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	g := p.session(sid)
	rows := &g.net
	if ssh {
		rows = &g.ssh
	}
	next := []vbox.Entry{e}
	for _, old := range *rows {
		if old != e {
			next = append(next, old)
		}
	}
	*rows = next
}

func (p *permissionState) grantCmd(sid, name string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	g := p.session(sid)
	for _, old := range g.cmd {
		if old == name {
			return
		}
	}
	g.cmd = append(g.cmd, name)
}

// ---- 读取面：FS/net/SSH/cmd 快照与判定 ----

// fsSnapshot 组合临时授权 + 基表 + 便利根（调用时取当次值）。
func (p *permissionState) fsSnapshot(sid string) vbox.FSRuleSet {
	return p.fsSnapshotWith(sid, nil)
}

// fsSnapshotForNative 在基表之下、便利根之前注入工作区元数据保护
// （.git/.aws 存在才保护；git 命令豁免）。
func (p *permissionState) fsSnapshotForNative(sid, workdir, cmd string) vbox.FSRuleSet {
	var protected []vbox.Rule
	if workdir != "" && filepath.Base(cmd) != "git" && filepath.Base(cmd) != "git.exe" {
		for _, name := range []string{".git", ".aws"} {
			target := filepath.Join(workdir, name)
			if st, err := os.Stat(target); err == nil && st.IsDir() {
				for _, pattern := range vbox.DualForms(target) {
					protected = append(protected, vbox.Rule{Pattern: pattern, Effect: vbox.EffRO, Class: vbox.ClassBuiltin})
				}
			}
		}
	}
	return p.fsSnapshotWith(sid, protected)
}

func (p *permissionState) fsSnapshotWith(sid string, protected []vbox.Rule) vbox.FSRuleSet {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.fsSnapshotLocked(sid, protected)
}

func (p *permissionState) fsSnapshotLocked(sid string, protected []vbox.Rule) vbox.FSRuleSet {
	// 快照自身 fail-closed：启动配置无效时连便利根也不放行——入口各自
	// 查 err() 是现状双保险，未来新调用点不再依赖该分散不变量。
	if p.invalid != nil {
		return vbox.FSRuleSet{DefaultWrite: vbox.EffDeny}
	}
	var rules []vbox.Rule
	if g := p.grants[sid]; g != nil {
		for _, target := range g.fs {
			for _, pattern := range vbox.DualForms(target) {
				rules = append(rules, vbox.Rule{Pattern: pattern, Effect: vbox.EffRW, Class: vbox.ClassTemp})
			}
		}
	}
	rules = append(rules, p.base.fsRules...)
	rules = append(rules, protected...)
	roots := append(append([]string(nil), p.baseRoots...), p.decideCaches...)
	if sid != "" && p.sessionDir != "" {
		roots = append(roots, p.sessionDir+"/"+sid)
	}
	for _, root := range vbox.DedupClean(roots) {
		rules = append(rules, vbox.Rule{Pattern: root, Effect: vbox.EffRW, Class: vbox.ClassConvenience})
	}
	fallback := vbox.EffDeny
	if p.base.fsOpen {
		fallback = vbox.EffRW
	}
	return vbox.FSRuleSet{Rules: rules, DefaultWrite: fallback}
}

func (p *permissionState) netSnapshot(sid string) vbox.NetRuleSet {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.netSnapshotLocked(sid)
}

func (p *permissionState) netSnapshotLocked(sid string) vbox.NetRuleSet {
	// 与 fs 快照同一不变量：启动配置无效时快照自身 fail-closed（含内建
	// localhost 放行也不下发）——未来新调用点不再依赖入口各自查 err()。
	if p.invalid != nil {
		return vbox.NetRuleSet{}
	}
	var rows []vbox.NetRule
	if g := p.grants[sid]; g != nil {
		for _, e := range g.net {
			rows = append(rows, vbox.NetRule{HostPort: e.String(), Allow: true})
		}
	}
	rows = append(rows, p.base.netRules...)
	rows = append(rows, netBuiltin...)
	return vbox.NetRuleSet{Rules: rows, Default: p.base.netOpen}
}

func (p *permissionState) sshSnapshot(sid string) vbox.NetRuleSet {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.sshSnapshotLocked(sid)
}

func (p *permissionState) sshSnapshotLocked(sid string) vbox.NetRuleSet {
	// 同上：invalid 时快照自身 fail-closed。
	if p.invalid != nil {
		return vbox.NetRuleSet{}
	}
	var rows []vbox.NetRule
	if g := p.grants[sid]; g != nil {
		for _, e := range g.ssh {
			rows = append(rows, vbox.NetRule{HostPort: e.String(), Allow: true})
		}
	}
	rows = append(rows, p.base.sshRules...)
	return vbox.NetRuleSet{Rules: rows, Default: p.base.sshOpen}
}

func (p *permissionState) sshAllowed(sid, hostport string) bool {
	if p.err() != nil {
		return false
	}
	return p.sshSnapshot(sid).Match(hostport)
}

// execAllowed 判定 cmd 域准入（会话 temp 授权行在基表之前，首命中）。
// 启动配置无效时一律拒绝。
func (p *permissionState) execAllowed(sid, name string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.invalid != nil {
		return false
	}
	var rows []string
	if g := p.grants[sid]; g != nil {
		for _, granted := range g.cmd {
			rows = append(rows, "allow:"+granted)
		}
	}
	rows = append(rows, p.base.execRules...)
	return cfg.CommandAllowed(p.base.execMode, rows, name)
}

// grantStatus 是 grant status 的执行体：四域姿态 + 规则表 + 会话级
// 临时授权（只读，不要求 grant_approved）。单锁一致快照。
func (p *permissionState) grantStatus(sessionKey string) string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	base := p.base
	var b strings.Builder
	// exec 域（policy/deny/allow 三键 + 会话级 cmd 授权）
	fmt.Fprintf(&b, "exec_policy: %s", base.execMode)
	fmt.Fprintf(&b, "\nexec_rules (%d): %s", len(base.execRules), strings.Join(base.execRules, " "))
	var session []string
	if g := p.grants[sessionKey]; g != nil {
		session = append(session, g.cmd...)
	}
	sort.Strings(session)
	fmt.Fprintf(&b, "\nsession cmd grants (%d, 重启失效): %s", len(session), strings.Join(session, " "))
	fmt.Fprintf(&b, "\n\nfs_policy: %s", policyName(base.fsOpen))
	fsRows := p.fsSnapshotLocked(sessionKey, nil).Rules
	fmt.Fprintf(&b, "\nfs_rules (%d, first match wins):", len(fsRows))
	for i, row := range fsRows {
		fmt.Fprintf(&b, "\n  %d. %s:%s [%s]", i+1, row.Effect, row.Pattern, row.Class)
	}
	for _, domain := range []struct {
		name string
		open bool
		rows vbox.NetRuleSet
	}{
		{"net", base.netOpen, p.netSnapshotLocked(sessionKey)},
		{"ssh", base.sshOpen, p.sshSnapshotLocked(sessionKey)},
	} {
		fmt.Fprintf(&b, "\n\n%s_policy: %s\n%s_rules (first match wins):", domain.name, policyName(domain.open), domain.name)
		for i, row := range domain.rows.Rules {
			effect := "deny"
			if row.Allow {
				effect = "allow"
			}
			fmt.Fprintf(&b, "\n  %d. %s:%s", i+1, effect, row.HostPort)
		}
	}
	return b.String()
}

func policyName(open bool) string {
	if open {
		return cfg.PolicyOpen
	}
	return cfg.PolicyDeny
}

func appendEnvDirs(dirs []string, names ...string) []string {
	for _, name := range names {
		if v := os.Getenv(name); v != "" {
			dirs = append(dirs, v)
		}
	}
	return dirs
}
