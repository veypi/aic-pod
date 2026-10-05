package host

import (
	"fmt"
	"github.com/veypi/aic-pod/protocol"
	"os"
	"path/filepath"
	"strings"

	"github.com/veypi/aic-pod/cfg"

	"github.com/veypi/vbox"
)

// grant 由引擎验证可信审批上下文后按 fs/net/ssh/cmd 分派。
// 临时授权写入 permissionState 会话内存；永久授权走受串行保护的更新：
// 构造候选配置 → 完整编译校验 → 成功保存 → 原子发布基表（任一步失败
// 均不发布部分规则）。两者均将允许行放在表头，首命中生效。
// 已启动进程继续使用启动时的沙箱快照。

func (c *Client) grantFS(sid, path string, permanent bool) (string, error) {
	wd, _ := os.Getwd()
	abs, err := protocol.ResolvePath(expandHomeDir(path), wd)
	if err != nil {
		return "", fmt.Errorf("exec grant fs: invalid path %q: %w", path, err)
	}
	abs = vbox.HostPathToOS(abs)
	if err = vbox.ValidateFSGrantTarget(abs); err != nil {
		return "", fmt.Errorf("exec grant fs: %w", err)
	}
	scope := "session"
	if permanent {
		if err = c.persistGrant("fs", abs); err != nil {
			return "", err
		}
		scope = "permanent"
	} else {
		c.perms.grantFS(sid, abs)
	}
	return fmt.Sprintf("granted fs write access: %s (scope=%s)", abs, scope), nil
}

func (c *Client) grantTarget(sid, domain, target string, permanent bool) (string, error) {
	entry, err := vbox.ParseEntry(target)
	if err != nil {
		return "", fmt.Errorf("exec grant %s: %w", domain, err)
	}
	scope := "session"
	if permanent {
		if err = c.persistGrant(domain, entry.String()); err != nil {
			return "", err
		}
		scope = "permanent"
	} else {
		c.perms.grantNet(sid, entry, domain == "ssh")
	}
	return fmt.Sprintf("granted %s access: %s (scope=%s)", domain, entry.String(), scope), nil
}

// persistGrant 将归一化后的允许行放到 <域>_rules 表头并去重：
// 构造候选配置 → 完整编译校验（compileAuth 覆盖四域全表）→ 原子保存 →
// 原子发布 permissionState 基表；任一步失败均不发布部分规则。
// 只修改文件配置，不把 flag/env 启动覆盖持久化；不触碰 cfg.Global
// （启动参数，不再是运行权限状态）。
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
		if err := cfg.ValidateCommandName(value); err != nil {
			return err
		}
		rows, rule = &fileCfg.ExecRules, "allow:"+value
	case "fs":
		value = vbox.Canonical(value)
		rows, rule = &fileCfg.FsRules, "rw:"+value
	case "net", "ssh":
		e, err := vbox.ParseEntry(value)
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
	// 完整编译校验先于保存：候选无效则不落盘、不发布。
	if err := fileCfg.ValidateAuth(); err != nil {
		return err
	}
	base, err := compileAuth(cfg.AuthFrom(fileCfg))
	if err != nil {
		return err
	}
	if err := cfg.Save(fileCfg); err != nil {
		return err
	}
	// 保存失败则不发布；发布即原子生效（即时生效能力保留）。
	c.perms.publish(base)
	return nil
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
	return c.perms.execAllowed(sid, name)
}
