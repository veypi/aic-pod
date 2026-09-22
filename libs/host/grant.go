package host

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/veypi/aic-pod/cfg"
	"github.com/veypi/aic-pod/libs/fsauth"
	"github.com/veypi/aic-pod/libs/netauth"
	"github.com/veypi/aic-pod/libs/policy"
	"github.com/veypi/aic-pod/libs/proto"
)

// grant（统一授权申请，四域同形）：
//
//	exec grant fs  <path>        [--temp|--permanent]
//	exec grant net <host:port>   [--temp|--permanent]
//	exec grant ssh <host[:port]> [--temp|--permanent]
//
// required 4（vcore 分级表）⇒ 必人工审批；批准后 granted 9 到达本函数。
//   - --temp（默认）：域 Policy 会话内存授权（重启失效、跨 session 失效）；
//     不追溯已启动的 bg 任务（沙箱白名单在 Start 时固化）。
//     规则表判定为 deny 终局的目标拒批——session 层不得放宽表判定的 deny（§2 硬底线）。
//   - --permanent：把规则行追加到 <域>_rules 表尾（fs 为 rw: 行、net/ssh 为
//     allow: 行，基于文件配置修改 + Save 落盘，与 set_config 同路径）——重启/跨
//     session 生效；覆盖 deny 行合法（机器是用户的），响应注明覆盖行号。
//   - 两档目标均过 §1 全域校验（fs 全域/家根/盘根不可授；net/ssh 通配 host 本身不可表达）。
func (c *Client) runGrant(sid, msgID string, argv []string) *proto.ToolResponse {
	domain, target, permanent, err := parseGrantArgv(argv)
	if err != nil {
		return &proto.ToolResponse{MsgID: msgID, State: proto.StateError, Error: "exec grant: " + err.Error()}
	}
	switch domain {
	case "exec":
		return c.grantExec(sid, msgID, target, permanent)
	case "fs":
		return c.grantFS(sid, msgID, target, permanent)
	case "net", "ssh":
		return c.grantTarget(sid, msgID, domain, target, permanent)
	}
	return &proto.ToolResponse{MsgID: msgID, State: proto.StateError,
		Error: fmt.Sprintf("exec grant: unknown domain %q (supported: fs, exec, net, ssh)", domain)}
}

// grantFS 处理 fs 域：路径写白名单申请（原 grant_apply 语义）。
func (c *Client) grantFS(sid, msgID, path string, permanent bool) *proto.ToolResponse {
	abs, err := filepath.Abs(expandHomeDir(path))
	if err != nil {
		return &proto.ToolResponse{MsgID: msgID, State: proto.StateError,
			Error: fmt.Sprintf("exec grant fs: invalid path %q: %v", path, err)}
	}
	if err := policy.ValidateFSGrantTarget(abs); err != nil {
		return &proto.ToolResponse{MsgID: msgID, State: proto.StateError, Error: "exec grant fs: " + err.Error()}
	}
	note := ""
	if permanent {
		if row, raw, ok := c.policy.LastDenyRow(abs); ok && c.policy.DenyHit(abs) {
			note = fmt.Sprintf("\nnote: this rule overrides the deny outcome from rule #%d (%s)", row, raw)
		}
	} else if c.policy.DenyHit(abs) {
		return &proto.ToolResponse{MsgID: msgID, State: proto.StateRejected,
			Error: fmt.Sprintf("exec grant fs: %s resolves to deny in the fs rule table and cannot be granted to a session (append a permanent rule through local management instead)", abs)}
	}
	scope := "session"
	if permanent {
		if err := c.persistGrant("fs", abs); err != nil {
			return &proto.ToolResponse{MsgID: msgID, State: proto.StateError,
				Error: "exec grant fs: persist: " + err.Error()}
		}
		scope = "permanent"
	} else {
		c.policy.Grant(sid, abs)
	}
	roots := c.policy.WriteRootsFor(sid)
	return &proto.ToolResponse{
		MsgID: msgID, State: proto.StateCompleted,
		Content: fmt.Sprintf("granted fs write access: %s (scope=%s, applies to fs writes and sandbox write binds)%s\ncurrent writable roots (%d):\n%s",
			abs, scope, note, len(roots), strings.Join(roots, "\n")),
		Attrs: map[string]string{"action": "grant", "domain": "fs", "target": abs, "scope": scope},
	}
}

// grantTarget 处理 net/ssh 域：host:port 目标白名单申请。
func (c *Client) grantTarget(sid, msgID, domain, target string, permanent bool) *proto.ToolResponse {
	e, err := netauth.ParseEntry(target)
	if err != nil {
		return &proto.ToolResponse{MsgID: msgID, State: proto.StateError,
			Error: fmt.Sprintf("exec grant %s: %v", domain, err)}
	}
	pol := c.netPol
	if domain == "ssh" {
		pol = c.sshPol
	}
	note := ""
	if permanent {
		if row, raw, ok := pol.LastDenyRow(e); ok && pol.DenyHit(e) {
			note = fmt.Sprintf("\nnote: this rule overrides the deny outcome from rule #%d (%s)", row, raw)
		}
	} else if pol.DenyHit(e) {
		return &proto.ToolResponse{MsgID: msgID, State: proto.StateRejected,
			Error: fmt.Sprintf("exec grant %s: %s resolves to deny in the %s_rules table and cannot be granted to a session (append a permanent rule through local management instead)", domain, e.String(), domain)}
	}
	scope := "session"
	if permanent {
		if err := c.persistGrant(domain, e.String()); err != nil {
			return &proto.ToolResponse{MsgID: msgID, State: proto.StateError,
				Error: fmt.Sprintf("exec grant %s: persist: %v", domain, err)}
		}
		scope = "permanent"
	} else {
		pol.Grant(sid, e)
	}
	list := pol.List(sid)
	return &proto.ToolResponse{
		MsgID: msgID, State: proto.StateCompleted,
		Content: fmt.Sprintf("granted %s access: %s (scope=%s)%s\ncurrent %s allow list (%d):\n%s",
			domain, e.String(), scope, note, domain, len(list), strings.Join(list, "\n")),
		Attrs: map[string]string{"action": "grant", "domain": domain, "target": e.String(), "scope": scope},
	}
}

// parseGrantArgv 解析 grant 参数：<域> <目标> [--temp|--permanent]（默认 --temp）。
func parseGrantArgv(argv []string) (domain, target string, permanent bool, err error) {
	for _, a := range argv {
		switch a {
		case "--temp":
			permanent = false
		case "--permanent":
			permanent = true
		default:
			if strings.HasPrefix(a, "-") {
				return "", "", false, fmt.Errorf("unknown flag %q (supported: --temp, --permanent)", a)
			}
			if domain == "" {
				domain = strings.ToLower(a)
				continue
			}
			if target != "" {
				return "", "", false, fmt.Errorf("multiple targets given: %q and %q", target, a)
			}
			target = a
		}
	}
	if domain == "" {
		return "", "", false, fmt.Errorf("domain is required (usage: grant <fs|exec|net|ssh> <target> [--temp|--permanent])")
	}
	if strings.TrimSpace(target) == "" {
		return "", "", false, fmt.Errorf("target is required (usage: grant %s <target> [--temp|--permanent])", domain)
	}
	return domain, target, permanent, nil
}

// persistGrant 把目标作为规则行追加到 <域>_rules 表尾并落盘（fs 为 rw: 行、
// net/ssh 为 allow: 行；基于文件配置修改——flag/env 启动覆盖不落盘，与 settings
// 同语义）；幂等（归一化口径下已存在跳过——macOS /var → /private/var 类 symlink、
// 端口零填充不再产生重复条目）。
func (c *Client) persistGrant(domain, value string) error {
	unlock := cfg.LockUpdate()
	defer unlock()
	fileCfg, err := cfg.LoadFile()
	if err != nil {
		return err
	}
	contains := func(list []string, norm func(string) string) bool {
		for _, p := range list {
			if norm(p) == norm(value) {
				return true
			}
		}
		return false
	}
	normEntry := func(s string) string {
		_, e, err := policy.ParseTargetRule(s)
		if err != nil {
			if e2, err2 := netauth.ParseEntry(s); err2 == nil {
				return e2.String()
			}
			return s
		}
		return e.String()
	}
	switch domain {
	case "exec":
		if contains(fileCfg.ExecAllow, func(s string) string { return s }) {
			return nil
		}
		fileCfg.ExecAllow = append(fileCfg.ExecAllow, value)
	case "fs":
		norm := func(s string) string {
			_, pat, err := policy.ParseFSRule(s)
			if err != nil {
				pat = s
			}
			return fsauth.Canonical(expandHomeDir(pat))
		}
		if contains(fileCfg.FsRules, norm) {
			c.syncAuth()
			return nil
		}
		fileCfg.FsRules = append(fileCfg.FsRules, "rw:"+value)
	case "net":
		if contains(fileCfg.NetRules, normEntry) {
			c.syncAuth()
			return nil
		}
		fileCfg.NetRules = append(fileCfg.NetRules, "allow:"+value)
	case "ssh":
		if contains(fileCfg.SshRules, normEntry) {
			c.syncAuth()
			return nil
		}
		fileCfg.SshRules = append(fileCfg.SshRules, "allow:"+value)
	default:
		return fmt.Errorf("unknown domain %q", domain)
	}
	if err := cfg.Save(fileCfg); err != nil {
		return err
	}
	cfg.SetAuth(cfg.AuthFrom(fileCfg))
	c.syncAuth()
	return nil
}

// syncAuth 重载三个域的 Policy（cfg.Global 已由调用方更新）。
func (c *Client) syncAuth() {
	c.policy.Reconcile()
	c.netPol.Reconcile()
	c.sshPol.Reconcile()
}

// expandHomeDir 展开路径的 ~ 前缀（与 api 包 expandHome 同语义；grant fs
// 面向 AI 直接调用，落盘前展开为真实绝对路径）。展开失败原样返回（后续
// Abs/canonical 会处理或报错）。
func expandHomeDir(p string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	if p == "~" {
		return home
	}
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(home, strings.TrimPrefix(p, "~/"))
	}
	return p
}

func (c *Client) execAllowed(sid, name string) bool {
	if cfg.CheckAuth() != nil {
		return false
	}
	a := cfg.AuthSnapshot()
	c.execGrantMu.RLock()
	allow := append(append([]string{"commands", "grant"}, a.ExecAllow...), c.execGrants[sid]...)
	c.execGrantMu.RUnlock()
	return policy.CommandAllowed(a.ExecPolicy, a.ExecDeny, allow, name)
}
func (c *Client) grantExec(sid, msgID, name string, permanent bool) *proto.ToolResponse {
	if err := policy.ValidateExec([]string{name}); err != nil {
		return reject(msgID, err.Error())
	}
	declared := c.tools != nil && c.tools.HasCommand(name)
	if !declared {
		return reject(msgID, "grant exec requires a registered command name")
	}
	a := cfg.AuthSnapshot()
	if !policy.CommandAllowed("open", a.ExecDeny, nil, name) {
		return reject(msgID, "command is in exec_deny and cannot be granted")
	}
	scope := "session"
	if permanent {
		if err := c.persistGrant("exec", name); err != nil {
			return reject(msgID, err.Error())
		}
		scope = "permanent"
	} else {
		c.execGrantMu.Lock()
		if c.execGrants == nil {
			c.execGrants = map[string][]string{}
		}
		found := false
		for _, old := range c.execGrants[sid] {
			if old == name {
				found = true
			}
		}
		if !found {
			c.execGrants[sid] = append(c.execGrants[sid], name)
		}
		c.execGrantMu.Unlock()
	}
	return &proto.ToolResponse{MsgID: msgID, State: proto.StateCompleted, Content: fmt.Sprintf("granted exec %s (scope=%s)", name, scope)}
}
