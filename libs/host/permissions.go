package host

// permissionState：host 唯一运行权限状态（四仓精简 §4，2026-10-06）。
// 一份已编译 FS/net/SSH/cmd 基表 + 一个 map[sessionID]sessionGrants
// （按字段保存四域临时授权），一把锁负责一致快照。FS、原生命令、MCP、
// SSH、grant status 全部从它读取；匹配算法由 vbox 提供。
//
// 基表生命周期：
//   - 启动：由 cfg.Global 按域编译（newPermissionState）；单个配置域无效
//     只使该域 fail-closed，其他域继续工作，客户端照常连接。
//   - permanent grant：受串行保护的更新（persistGrant）——构造候选配置 →
//     完整编译校验（compileAuth）→ 成功保存 → 原子发布（publish）；
//     任一步失败均不发布部分规则。
//   - 临时 grant：只更新 grants 字段（会话隔离，重启失效）。
//
// cfg.Global 只是启动参数，不再是第二份并发运行权限状态；运行中修改
// cfg.Global 不影响本状态（设置继续重启生效）。
import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"

	"github.com/veypi/aic-pod/cfg"
	"github.com/veypi/aic-pod/libs/execution"
	"github.com/veypi/vbox"
	vshpolicy "github.com/veypi/vsh/policy"
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
	fsOpen           bool
	fsRules          []vbox.Rule // cfg 规则 + 内建 deny（首命中）
	netOpen          bool
	netRules         []vbox.NetRule
	netUpstreamProxy string
	sshOpen          bool
	sshRules         []vbox.NetRule
	execMode         string
	execRules        []string
	errs             map[string]error // 每个配置域独立的语法/编译错误
}

type permissionState struct {
	mu                            sync.RWMutex
	workDir, sessionDir, stateDir string
	baseRoots, decideCaches       []string
	base                          compiledAuth
	grants                        map[string]*sessionGrants
}

// netBuiltin 是 net 域内建放行（本机回环）。
var netBuiltin = []vbox.NetRule{{HostPort: "localhost:*", Allow: true}}

// sshBuiltin 是 ssh 域内建放行：github.com:22（git over ssh 是最高频托管
// ssh 目标，免每会话 grant）。排在快照表尾——所有者的 cfg deny 行首命中
// 仍可压过它。
var sshBuiltin = []vbox.NetRule{{HostPort: "github.com:22", Allow: true}}

// nativeNetPlatform 是原生沙箱能力校验用的平台标识（默认本机；测试可注入）。
// net 规则的「内核能否落实」按平台分流（vbox.CheckNetPolicy），配置期校验与
// grant status 标注共用同一取值。
var nativeNetPlatform = runtime.GOOS

// compileAuth 完整编译校验一份授权配置（纯函数，不触碰运行状态）。
func compileAuth(a cfg.AuthCfg) (*compiledAuth, error) {
	base := &compiledAuth{
		fsOpen:           a.FsPolicy == cfg.PolicyOpen,
		netOpen:          a.NetPolicy == cfg.PolicyOpen,
		netUpstreamProxy: a.NetUpstreamProxy,
		sshOpen:          a.SshPolicy == cfg.PolicyOpen,
		execMode:         a.ExecPolicy,
		execRules:        append([]string(nil), a.ExecRules...),
		errs:             map[string]error{},
	}
	if err := validateAuthPolicy(a.ExecPolicy); err != nil {
		base.setError("exec", "exec_policy", err)
	} else if err := cfg.ValidateExecRules(a.ExecRules); err != nil {
		base.setError("exec", "exec_rules", err)
	}
	if err := validateAuthPolicy(a.FsPolicy); err != nil {
		base.setError("fs", "fs_policy", err)
	} else if rules, err := compileFSBase(a.FsRules); err != nil {
		base.setError("fs", "fs_rules", err)
	} else {
		base.fsRules = rules
	}
	if err := validateAuthPolicy(a.NetPolicy); err != nil {
		base.setError("net", "net_policy", err)
	} else if rules, err := compileNetRules(a.NetRules); err != nil {
		base.setError("net", "net_rules", err)
	} else {
		base.netRules = rules
	}
	if base.errs["net"] == nil && a.NetUpstreamProxy != "" {
		if err := vbox.ValidateUpstreamProxy(a.NetUpstreamProxy); err != nil {
			base.setError("net", "net_upstream_proxy", err)
		}
	}
	if err := validateAuthPolicy(a.SshPolicy); err != nil {
		base.setError("ssh", "ssh_policy", err)
	} else if rules, err := compileNetRules(a.SshRules); err != nil {
		base.setError("ssh", "ssh_rules", err)
	} else {
		base.sshRules = rules
	}
	return base, base.err()
}

func compileFSBase(raws []string) ([]vbox.Rule, error) {
	rules, err := vbox.CompileFSRules(raws, vbox.ClassCfg)
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
	hard := hardProtectionPaths()
	for i := range hard {
		hard[i] = "!deny:" + hard[i]
	}
	protected, err := vbox.CompileFSRules(hard, vbox.ClassBuiltin)
	if err != nil {
		return nil, err
	}
	return append(append(rules, builtin...), protected...), nil
}

func validateAuthPolicy(value string) error {
	if value != "" && value != cfg.PolicyOpen && value != cfg.PolicyDeny {
		return fmt.Errorf("invalid policy %q (want deny | open)", value)
	}
	return nil
}

func (b *compiledAuth) setError(domain, field string, err error) {
	if err != nil {
		b.errs[domain] = &cfg.AuthFieldError{Field: field, Err: err}
	}
}

func (b *compiledAuth) err() error {
	for _, domain := range []string{"exec", "fs", "net", "ssh"} {
		if err := b.errs[domain]; err != nil {
			return err
		}
	}
	return nil
}

func compileNetRules(rows []string) ([]vbox.NetRule, error) {
	var compiled []vbox.NetRule
	for _, raw := range rows {
		r, err := vbox.ParseNetRule(raw)
		if err != nil {
			return nil, err
		}
		compiled = append(compiled, r)
	}
	return compiled, nil
}

// newPermissionState 构建启动权限状态。StateDir 失败是环境错误；每个授权
// 配置域独立编译，错误只使对应快照 fail-closed，不阻断其他域或设备连接。
func newPermissionState(workDir string, o *cfg.Options) (*permissionState, error) {
	p := &permissionState{grants: map[string]*sessionGrants{}}
	dir, err := cfg.StateDir()
	if err != nil {
		return nil, err
	}
	p.stateDir = vbox.Canonical(dir)
	p.sessionDir = p.stateDir + "/sessions"
	p.setWorkDir(workDir)
	auth := cfg.AuthFrom(o)
	// 非空 policy 保留原值参与校验；空值才使用 AuthFrom 的默认姿态。
	if o.ExecPolicy != "" {
		auth.ExecPolicy = o.ExecPolicy
	}
	if o.FsPolicy != "" {
		auth.FsPolicy = o.FsPolicy
	}
	if o.NetPolicy != "" {
		auth.NetPolicy = o.NetPolicy
	}
	if o.SshPolicy != "" {
		auth.SshPolicy = o.SshPolicy
	}
	base, _ := compileAuth(auth)
	p.base = *base
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
}

// err 返回指定配置域的错误；其他域不受影响。
func (p *permissionState) err(domain string) error {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.base.errs[domain]
}

func (p *permissionState) session(sid string) *sessionGrants {
	g := p.grants[sid]
	if g == nil {
		g = &sessionGrants{}
		p.grants[sid] = g
	}
	return g
}

// ---- 临时授权（会话隔离；level 0，同 level 按原表序） ----

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
	if err := p.base.errs["fs"]; err != nil {
		return vbox.FSRuleSet{DefaultWrite: vbox.EffDeny, Err: err}
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
	if p.base.errs["net"] != nil {
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
	return vbox.NetRuleSet{Rules: rows, Default: p.base.netOpen, UpstreamProxy: p.base.netUpstreamProxy}
}

func (p *permissionState) sshSnapshot(sid string) vbox.NetRuleSet {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.sshSnapshotLocked(sid)
}

func (p *permissionState) sshSnapshotLocked(sid string) vbox.NetRuleSet {
	if p.base.errs["ssh"] != nil {
		return vbox.NetRuleSet{}
	}
	var rows []vbox.NetRule
	if g := p.grants[sid]; g != nil {
		for _, e := range g.ssh {
			rows = append(rows, vbox.NetRule{HostPort: e.String(), Allow: true})
		}
	}
	rows = append(rows, p.base.sshRules...)
	rows = append(rows, sshBuiltin...)
	return vbox.NetRuleSet{Rules: rows, Default: p.base.sshOpen}
}

func (p *permissionState) sshAllowed(sid, hostport string) bool {
	if p.err("ssh") != nil {
		return false
	}
	return p.sshSnapshot(sid).Match(hostport)
}

// AllowExec 是 vsh 的统一命令准入入口。解析名和解析路径使用同一张 exec 表；
// 命令实现、来源和内部策略不改变这里的判定。
func (p *permissionState) AllowExec(ctx context.Context, req vshpolicy.ExecRequest) error {
	if !p.execAllowedIdentity(execution.SessionFromContext(ctx), req.Name, req.Path) {
		return &vshpolicy.DeniedError{Subject: fmt.Sprintf("command %q", req.Name), Reason: "not in allowlist"}
	}
	return nil
}

func (p *permissionState) execAllowed(sid, name string) bool {
	return p.execAllowedIdentity(sid, name, "")
}

func (p *permissionState) execAllowedIdentity(sid, name, resolvedPath string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.base.errs["exec"] != nil {
		return false
	}
	var rows []string
	if g := p.grants[sid]; g != nil {
		for _, granted := range g.cmd {
			rows = append(rows, "allow:"+granted)
		}
	}
	rows = append(rows, p.base.execRules...)
	identities := []string{name}
	if resolvedPath != "" && resolvedPath != name {
		identities = append(identities, resolvedPath)
	}
	return cfg.CommandAllowed(p.base.execMode, rows, identities...)
}

// sandboxWritableProbe 是「该路径能否在 OS 沙箱内被授为可写根」的探测入口
// （默认 vbox.CheckFSGrantTarget，非 Windows 平台恒 nil）。两个调用点：grant fs
// 的回执警告（grant.go）、grant status 的 rw 行标注（本文件）；测试用它注入
// 「授不上」的结果。探测自身先做存在性判断、再做一次对象打开（vbox 侧 os.Stat +
// CreateFile），调用方不需要重复形态预检。
var sandboxWritableProbe = vbox.CheckFSGrantTarget

// writeRootProbeLimit 限制一次 grant status 的探测路径数：每次探测要开一次对象
// 句柄，而 rw 行可能指向已不可达的网络共享（CreateFile 会一路阻塞到 SMB 超时）。
// 规则表远超这个量级时只探前面的路径，其余在末尾统一说明。
var writeRootProbeLimit = 32

// writeRootProbeCache 是一次 grant status 内的探测缓存：同一路径只探一次，且总
// 探测路径数不超过 writeRootProbeLimit（超出的记进 skipped，由调用方汇总说明）。
type writeRootProbeCache struct {
	notes   map[string]string
	skipped map[string]bool
}

func newWriteRootProbeCache() *writeRootProbeCache {
	return &writeRootProbeCache{notes: map[string]string{}, skipped: map[string]bool{}}
}

// note 标注一条 rw 行在 OS 沙箱内是否真的可写：不可写时返回一行提示，可写 /
// 无法判定（不存在、通配）返回空串。口径与 vbox 的可写根派生一致：剥 /** 后缀、
// 跳过含通配的模式——只有这些行会变成 OS 层可写根。降级是安静的，这条标注就是
// 它的可发现性渠道（另两条是 vbox 的 write-root 日志与 grant fs 的警告）。
func (c *writeRootProbeCache) note(row vbox.Rule) string {
	if row.Effect != vbox.EffRW {
		return ""
	}
	p := strings.TrimSuffix(row.Pattern, "/**")
	if p == "" || strings.ContainsAny(p, "*?") {
		return ""
	}
	if note, ok := c.notes[p]; ok {
		return note
	}
	if len(c.notes) >= writeRootProbeLimit {
		c.skipped[p] = true
		return ""
	}
	note := ""
	if err := sandboxWritableProbe(vbox.HostPathToOS(p)); err != nil {
		note = " [沙箱内不可写: " + err.Error() + "]"
	}
	c.notes[p] = note
	return note
}

// skippedNote 汇总本次因上限而未探测的 rw 行（没有则返回空串）。
func (c *writeRootProbeCache) skippedNote() string {
	if len(c.skipped) == 0 {
		return ""
	}
	return fmt.Sprintf("\n  (另有 %d 条 rw 行未做沙箱可写性探测：单次上限 %d)", len(c.skipped), writeRootProbeLimit)
}

// grantStatus 是 grant status 的执行体：四域姿态 + 规则表 + 会话级
// 临时授权（只读，不要求 grant_approved）。单锁一致快照：锁内只取快照，格式化
// （含 rw 行的沙箱可写性探测——要开对象句柄，可能撞上不可达的网络路径）一律在
// 锁外，探测的等待不该按住读锁、挡住并发的 grant/publish。
func (p *permissionState) grantStatus(sessionKey string) string {
	type netDomain struct {
		name string
		open bool
		rows vbox.NetRuleSet
	}

	p.mu.RLock()
	base := p.base
	var session []string
	if g := p.grants[sessionKey]; g != nil {
		session = append(session, g.cmd...)
	}
	fsRows := p.fsSnapshotLocked(sessionKey, nil).Rules
	domains := []netDomain{
		{"net", base.netOpen, p.netSnapshotLocked(sessionKey)},
		{"ssh", base.sshOpen, p.sshSnapshotLocked(sessionKey)},
	}
	// net 域的完整快照在锁外做原生落地诊断；诊断不改变规则或阻塞其他域。
	netNative := domains[0].rows
	p.mu.RUnlock()

	netLandNotes := map[int]string{}
	landings, landErr := vbox.CheckNetPolicy(nativeNetPlatform, netNative)
	if landErr == nil {
		for i, l := range landings {
			var kinds []string
			if l.Kernel {
				kinds = append(kinds, "kernel")
			}
			if l.Proxy {
				kinds = append(kinds, "proxy")
			}
			if l.Reserved {
				kinds = append(kinds, "reserved")
			}
			if l.Absorbed {
				kinds = append(kinds, "absorbed")
			}
			if len(kinds) > 0 {
				netLandNotes[i] = " [" + strings.Join(kinds, "+") + ": " + l.Reason + "]"
			}
		}
	}

	sort.Strings(session)
	var b strings.Builder
	// exec 域（policy/deny/allow 三键 + 会话级 cmd 授权）
	fmt.Fprintf(&b, "exec_policy: %s", base.execMode)
	fmt.Fprintf(&b, "\nexec_rules (%d): %s", len(base.execRules), strings.Join(base.execRules, " "))
	fmt.Fprintf(&b, "\nsession cmd grants (%d, 重启失效): %s", len(session), strings.Join(session, " "))
	for _, domain := range []string{"exec", "fs", "net", "ssh"} {
		if err := base.errs[domain]; err != nil {
			fmt.Fprintf(&b, "\n%s_config_error: %v", domain, err)
		}
	}
	fmt.Fprintf(&b, "\n\nfs_policy: %s", policyName(base.fsOpen))
	fmt.Fprintf(&b, "\nfs_rules (%d, level + original order):", len(fsRows))
	probes := newWriteRootProbeCache()
	for i, row := range fsRows {
		fmt.Fprintf(&b, "\n  %d. %s:%s [%s level=%d]%s", i+1, row.Effect, row.Pattern, row.Class, row.Level, probes.note(row))
	}
	b.WriteString(probes.skippedNote())
	for _, domain := range domains {
		fmt.Fprintf(&b, "\n\n%s_policy: %s\n%s_rules (level + original order):", domain.name, policyName(domain.open), domain.name)
		if domain.name == "net" && landErr != nil {
			fmt.Fprintf(&b, "\n  [原生沙箱无法落实当前规则表: %v]", landErr)
		}
		for i, row := range domain.rows.Rules {
			note := ""
			if domain.name == "net" {
				note = netLandNotes[i]
			}
			fmt.Fprintf(&b, "\n  %d. %s%s", i+1, row.String(), note)
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
