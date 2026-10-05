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

func (c *Client) grantFS(sid, path string, permanent bool) (string, error) {
	wd, _ := os.Getwd()
	abs, err := proto.ResolvePath(expandHomeDir(path), wd, nil)
	if err != nil {
		return "", fmt.Errorf("exec grant fs: invalid path %q: %w", path, err)
	}
	abs = proto.HostPathToOS(abs)
	if err = policy.ValidateFSGrantTarget(abs); err != nil {
		return "", fmt.Errorf("exec grant fs: %w", err)
	}
	scope := "session"
	if permanent {
		if err = c.persistGrant("fs", abs); err != nil {
			return "", err
		}
		scope = "permanent"
	} else {
		c.policy.Grant(sid, abs)
	}
	return fmt.Sprintf("granted fs write access: %s (scope=%s)", abs, scope), nil
}
func (c *Client) grantTarget(sid, domain, target string, permanent bool) (string, error) {
	entry, err := netauth.ParseEntry(target)
	if err != nil {
		return "", fmt.Errorf("exec grant %s: %w", domain, err)
	}
	pol := c.netPol
	if domain == "ssh" {
		pol = c.sshPol
	}
	scope := "session"
	if permanent {
		if err = c.persistGrant(domain, entry.String()); err != nil {
			return "", err
		}
		scope = "permanent"
	} else {
		pol.Grant(sid, entry)
	}
	return fmt.Sprintf("granted %s access: %s (scope=%s)", domain, entry.String(), scope), nil
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
