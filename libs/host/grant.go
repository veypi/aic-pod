package host

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/veypi/aic-pod/cfg"
	"github.com/veypi/aic-pod/libs/fsauth"
	"github.com/veypi/aic-pod/libs/netauth"
	"github.com/veypi/aic-pod/libs/proto"
)

// grant（统一授权申请，三域同形）：
//
//	exec grant fs  <path>        [--temp|--permanent]
//	exec grant net <host:port>   [--temp|--permanent]
//	exec grant ssh <host[:port]> [--temp|--permanent]
//
// required 4（vcore 分级表）⇒ 必人工审批；批准后 granted 9 到达本函数。
//   - --temp（默认）：域 Policy 会话内存授权（重启失效、跨 session 失效）；
//     不追溯已启动的 bg 任务（沙箱白名单在 Start 时固化）。
//   - --permanent：写 cfg 对应 allow 列表（fs_allow/net_allow/ssh_allow，
//     基于文件配置修改 + Save 落盘，与 set_config 同路径）——重启/跨 session 生效。
//   - 域 deny 名单内的目标拒绝申请（fs 路径 Policy.DenyHit 校验；
//     net/ssh 目标 Policy.DenyHit 条目重叠判定）。
func (c *Client) runGrant(sid, msgID string, argv []string) *proto.ToolResponse {
	domain, target, permanent, err := parseGrantArgv(argv)
	if err != nil {
		return &proto.ToolResponse{MsgID: msgID, State: proto.StateError, Error: "exec grant: " + err.Error()}
	}
	switch domain {
	case "fs":
		return c.grantFS(sid, msgID, target, permanent)
	case "net", "ssh":
		return c.grantTarget(sid, msgID, domain, target, permanent)
	}
	return &proto.ToolResponse{MsgID: msgID, State: proto.StateError,
		Error: fmt.Sprintf("exec grant: unknown domain %q (supported: fs, net, ssh)", domain)}
}

// grantFS 处理 fs 域：路径写白名单申请（原 grant_apply 语义）。
func (c *Client) grantFS(sid, msgID, path string, permanent bool) *proto.ToolResponse {
	abs, err := filepath.Abs(expandHomeDir(path))
	if err != nil {
		return &proto.ToolResponse{MsgID: msgID, State: proto.StateError,
			Error: fmt.Sprintf("exec grant fs: invalid path %q: %v", path, err)}
	}
	if c.policy.DenyHit(abs) {
		return &proto.ToolResponse{MsgID: msgID, State: proto.StateRejected,
			Error: fmt.Sprintf("exec grant fs: %s is in the fs_deny list and cannot be granted (deny entries are not approval-able; add an explicit fs_allow entry in host config — bare path or glob like **/.env — to override)", abs)}
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
		Content: fmt.Sprintf("granted fs write access: %s (scope=%s, applies to fs writes and sandbox write binds)\ncurrent writable roots (%d):\n%s",
			abs, scope, len(roots), strings.Join(roots, "\n")),
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
	if pol.DenyHit(e) {
		return &proto.ToolResponse{MsgID: msgID, State: proto.StateRejected,
			Error: fmt.Sprintf("exec grant %s: %s is in the %s_deny list and cannot be granted (deny entries are not approval-able; add a narrower allow entry in host config to override)", domain, e.String(), domain)}
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
		Content: fmt.Sprintf("granted %s access: %s (scope=%s)\ncurrent %s allow list (%d):\n%s",
			domain, e.String(), scope, domain, len(list), strings.Join(list, "\n")),
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
		return "", "", false, fmt.Errorf("domain is required (usage: grant <fs|net|ssh> <target> [--temp|--permanent])")
	}
	if strings.TrimSpace(target) == "" {
		return "", "", false, fmt.Errorf("target is required (usage: grant %s <target> [--temp|--permanent])", domain)
	}
	return domain, target, permanent, nil
}

// persistGrant 把目标追加进 cfg 对应域的 allow 列表并落盘（基于文件配置修改——
// flag/env 启动覆盖不落盘，与 api.SetConfig 同语义）；幂等（归一化口径下
// 已存在跳过——macOS /var → /private/var 类 symlink、端口零填充不再产生重复条目）。
func (c *Client) persistGrant(domain, value string) error {
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
		if e, err := netauth.ParseEntry(s); err == nil {
			return e.String()
		}
		return s
	}
	switch domain {
	case "fs":
		norm := func(s string) string { return fsauth.Canonical(expandHomeDir(s)) }
		if contains(fileCfg.FsAllow, norm) {
			c.syncAuth()
			return nil
		}
		fileCfg.FsAllow = append(fileCfg.FsAllow, value)
	case "net":
		if contains(fileCfg.NetAllow, normEntry) {
			c.syncAuth()
			return nil
		}
		fileCfg.NetAllow = append(fileCfg.NetAllow, value)
	case "ssh":
		if contains(fileCfg.SshAllow, normEntry) {
			c.syncAuth()
			return nil
		}
		fileCfg.SshAllow = append(fileCfg.SshAllow, value)
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
