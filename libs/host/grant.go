package host

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/veypi/aic-pod/cfg"
	"github.com/veypi/aic-pod/libs/netauth"
	"github.com/veypi/aic-pod/libs/policy"
	"github.com/veypi/aic-pod/libs/proto"
	"github.com/veypi/vbox"
)

// grant 由引擎验证可信审批上下文后按 fs/net/ssh/cmd 分派。
// 临时授权写入会话内存，永久授权写入对应配置规则表；两者均将允许行
// 放在表头，首命中生效。已启动进程继续使用启动时的沙箱快照。

// grantFS 处理 fs 域：路径写白名单申请（原 grant_apply 语义）。
func (c *Client) grantFS(sid, msgID, path string, permanent bool) *proto.ToolResponse {
	// 路径解析走 proto 可解析层（与规则匹配/执行层同口径：/c/ 规范形、/tmp
	// 虚拟别名、旧盘符形态容错归一；相对路径按 pod 进程 cwd 展开，同历史
	// filepath.Abs 行为），出口转原生 OS 路径供护栏校验与授权落盘——win 上
	// filepath.Abs("/c/…") 会错拼成 <当前盘>:\c\…（2026-09-28 验收报告）。
	wd, _ := os.Getwd()
	abs, err := proto.ResolvePath(expandHomeDir(path), wd, nil)
	if err != nil {
		return &proto.ToolResponse{MsgID: msgID, State: proto.StateError,
			Error: fmt.Sprintf("exec grant fs: invalid path %q: %v", path, err)}
	}
	abs = proto.HostPathToOS(abs)
	if err := policy.ValidateFSGrantTarget(abs); err != nil {
		return &proto.ToolResponse{MsgID: msgID, State: proto.StateError, Error: "exec grant fs: " + err.Error()}
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

	return &proto.ToolResponse{
		MsgID: msgID, State: proto.StateCompleted,
		Content: fmt.Sprintf("granted fs write access: %s (scope=%s)", abs, scope),
		Attrs:   map[string]string{"action": "grant", "domain": "fs", "target": abs, "scope": scope},
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

	return &proto.ToolResponse{
		MsgID: msgID, State: proto.StateCompleted,
		Content: fmt.Sprintf("granted %s access: %s (scope=%s)", domain, e.String(), scope),
		Attrs:   map[string]string{"action": "grant", "domain": domain, "target": e.String(), "scope": scope},
	}
}

// persistGrant 将归一化后的允许行放到 <域>_rules 表头并去重、落盘。
// 只修改文件配置，不把 flag/env 启动覆盖持久化。
func (c *Client) persistGrant(domain, value string) error {
	unlock := cfg.LockUpdate()
	defer unlock()
	fileCfg, err := cfg.LoadFile()
	if err != nil {
		return err
	}
	var rows *[]string
	var rule string
	switch domain {
	case "exec":
		if err := policy.ValidateCommandName(value); err != nil {
			return err
		}
		rows, rule = &fileCfg.ExecRules, "allow:"+value
	case "fs":
		value = vbox.Canonical(value)
		rows, rule = &fileCfg.FsRules, "rw:"+value
	case "net", "ssh":
		e, err := policy.ParseEntry(value)
		if err != nil {
			return err
		}
		rule = "allow:" + e.String()
		if domain == "net" {
			rows = &fileCfg.NetRules
		} else {
			rows = &fileCfg.SshRules
		}
	default:
		return fmt.Errorf("unknown domain %q", domain)
	}
	// Move an identical grant to the front; an older deny may have preceded it.
	next := []string{rule}
	for _, row := range *rows {
		if row != rule {
			next = append(next, row)
		}
	}
	*rows = next
	if err := fileCfg.ValidateAuth(); err != nil {
		return err
	}
	if err := cfg.Save(fileCfg); err != nil {
		return err
	}
	cfg.SetAuth(cfg.AuthFrom(fileCfg))
	return c.syncAuth()
}

// syncAuth 重载三个域的 Policy（cfg.Global 已由调用方更新）。
func (c *Client) syncAuth() error {
	if err := cfg.CheckAuth(); err != nil {
		return err
	}
	if err := c.policy.Reconcile(); err != nil {
		return err
	}
	if err := c.netPol.Reconcile(); err != nil {
		return err
	}
	return c.sshPol.Reconcile()
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
	rows := make([]string, 0, len(c.execGrants[sid])+len(a.ExecRules))
	for _, granted := range c.execGrants[sid] {
		rows = append(rows, "allow:"+granted)
	}
	c.execGrantMu.RUnlock()
	rows = append(rows, a.ExecRules...)
	return policy.CommandAllowed(a.ExecPolicy, rows, name)
}
