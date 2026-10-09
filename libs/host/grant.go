package host

import (
	"fmt"
	"github.com/veypi/aic-pod/protocol"
	"os"
	"os/user"
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
	// 沙箱内可写性探测（**只警告，不拒绝**）：目录属主不是当前用户时（典型
	// Administrators/SYSTEM 而当前用户只有 Modify），能力 SID 的 ACE 落不下去
	// ——授权本身仍然成立（进程内 fs 工具用宿主令牌写，不需要 WRITE_DAC），
	// 但 spawn 出来的子进程在沙箱内写该目录会被拒。在授权这一刻就告知，别把
	// 问题埋到之后每一次 exec 上（vbox 侧已按根降级，不会再打死 exec）。
	sandboxErr := sandboxWritableProbe(abs)
	scope := "session"
	if permanent {
		if err = c.persistGrant("fs", abs); err != nil {
			return "", err
		}
		scope = "permanent"
	} else {
		c.perms.grantFS(sid, abs)
	}
	return grantFSResult(abs, scope, sandboxErr), nil
}

// grantFSResult 组装 grant fs 的成功回执；sandboxErr 非 nil 表示该目录在 OS
// 沙箱内授不上写权限（仅警告：授权已生效，工具层可用）。
func grantFSResult(abs, scope string, sandboxErr error) string {
	msg := fmt.Sprintf("granted fs write access: %s (scope=%s)", abs, scope)
	if sandboxErr == nil {
		return msg
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n  warn: 该目录在沙箱内授不上写权限（spawn 的子进程写会被拒；vsh fs 工具不受影响）：%v", msg, sandboxErr)
	b.WriteString("\n  原因：通常是没有 WRITE_DAC——目录属主是 Administrators/SYSTEM，当前用户只有 Modify（Modify 不含改安全描述符的权限）")
	b.WriteString("\n  处置（需管理员，任选其一）：")
	if who := currentUserName(); who != "" {
		// 注意不能对路径用 %q：Go 会转义反斜杠（C:\\models\\llama），用户复制即错。
		fmt.Fprintf(&b, "\n    1) icacls \"%s\" /setowner \"%s\"            （把属主改成当前用户）", abs, who)
		fmt.Fprintf(&b, "\n    2) icacls \"%s\" /grant \"%s:(OI)(CI)F\"    （只给当前用户完全控制）", abs, who)
	} else {
		b.WriteString("\n    把该目录的属主或完全控制交给当前用户")
	}
	b.WriteString("\n  不要用 Authenticated Users（*S-1-5-11）：那等于把完全控制（含改 ACL）给所有已认证主体")
	return b.String()
}

// currentUserName 返回当前宿主用户名（icacls 可直接使用的形式）；取不到返回空串。
func currentUserName() string {
	u, err := user.Current()
	if err != nil || u == nil {
		return ""
	}
	return u.Username
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
