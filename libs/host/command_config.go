package host

// command_config.go — commands config：命令定义级配置的统一写面。
//
// 一张表装两件事（它们本来就是一件事——「这条命令定义的运行配置」）：
//   - 应用配置 env：命令进程的环境变量；
//   - 沙箱策略 no_sandbox：逃出 pod 沙箱（提权）。
//
// 存储：cfg.Options.CommandConfig，key = 定义身份（skill:<skill_id>:<name> 或
// config:<alias>）——不是可被同名替换的 alias：同名替换不继承配置。
//
// 信任级：
//   - env：AI 可直接写（只是环境变量，不越权）。
//   - no_sandbox：来自平台的会话必须带平台审批事实（execution.GrantApproved）——
//     平台侧的人工批准通道；设备 owner 在本机直接执行不需要审批。
//
// 生效：落盘配置文件 + 热加载重建注册表（常驻进程按新 env 重启），不需重启后端。

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/veypi/aic-pod/cfg"
	"github.com/veypi/aic-pod/libs/execution"
	"github.com/veypi/vsh/commands"
)

const commandConfigHelp = `usage: commands config <alias> [K=V ...] [--unset K] [--no-sandbox|--sandbox] [--clear] [--json]
  查看或修改一条命令定义的运行配置（按定义身份存储，与 alias 同名替换无关）。

  commands config <alias>                     显示当前配置
  commands config <alias> K=V ...             设置应用配置（环境变量）
  commands config <alias> --unset K           删除一个环境变量
  commands config <alias> --no-sandbox        免沙箱运行（提权：需平台审批）
  commands config <alias> --sandbox           取消免沙箱
  commands config <alias> --clear             清空该定义的全部配置
  commands config <alias> --json              以 JSON 输出`

// hostCommandConfig 是 `commands config` 的执行体。
func (c *Client) hostCommandConfig(ctx context.Context, inv *commands.Invocation, args []string) error {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		fmt.Fprintln(inv.Stdout, commandConfigHelp)
		return nil
	}
	alias := strings.TrimSpace(args[0])
	def, ok := c.cmds.definition(alias)
	if !ok {
		return commands.Exitf(inv, 2, "commands config: unknown command %q", alias)
	}

	env := map[string]string{}
	var unset []string
	var noSandbox *bool
	clear := false
	asJSON := false
	for n := 0; n < len(args[1:]); n++ {
		arg := args[1+n]
		switch {
		case arg == "--json":
			asJSON = true
		case arg == "--clear":
			clear = true
		case arg == "--no-sandbox":
			value := true
			noSandbox = &value
		case arg == "--sandbox":
			value := false
			noSandbox = &value
		case arg == "--unset":
			n++
			if 1+n >= len(args) {
				return commands.Exitf(inv, 2, "commands config: --unset requires a key")
			}
			unset = append(unset, args[1+n])
		case strings.HasPrefix(arg, "--"):
			return commands.Exitf(inv, 2, "commands config: unknown option %q", arg)
		default:
			key, value, ok := strings.Cut(arg, "=")
			key = strings.TrimSpace(key)
			if !ok || key == "" || strings.ContainsAny(key, "=\x00") || strings.ContainsRune(value, 0) {
				return commands.Exitf(inv, 2, "commands config: expected K=V (got %q)", arg)
			}
			env[key] = value
		}
	}

	if !clear && len(env) == 0 && len(unset) == 0 && noSandbox == nil {
		return c.printCommandConfig(inv, def, asJSON)
	}
	// 提权项：来自平台的会话必须带平台审批事实（人工批准通道）；
	// 设备 owner 本机直接执行（无平台归属）不需要审批。
	if noSandbox != nil && *noSandbox &&
		execution.OwnerFromContext(ctx) != "" && !execution.GrantApprovedFromContext(ctx) {
		return commands.Exitf(inv, 1, "commands config: no_sandbox 需要 owner 批准（平台审批后重试）")
	}

	id := def.ID
	if err := c.updateCommandConfig(id, func(entry *cfg.CommandDefConfig) {
		if clear {
			*entry = cfg.CommandDefConfig{}
		}
		for key, value := range env {
			if entry.Env == nil {
				entry.Env = map[string]string{}
			}
			entry.Env[key] = value
		}
		for _, key := range unset {
			delete(entry.Env, key)
		}
		if noSandbox != nil {
			entry.NoSandbox = *noSandbox
		}
	}); err != nil {
		return commands.Exitf(inv, 1, "commands config: %s", err)
	}
	if err := c.cmds.reload(); err != nil {
		return commands.Exitf(inv, 1, "commands config: reload: %s", err)
	}
	updated, ok := c.cmds.definition(alias)
	if !ok {
		return commands.Exitf(inv, 1, "commands config: command %q disappeared after reload", alias)
	}
	return c.printCommandConfig(inv, updated, asJSON)
}

// updateCommandConfig 落盘一条定义级配置（文件锁内读改写）。
func (c *Client) updateCommandConfig(id string, mutate func(*cfg.CommandDefConfig)) error {
	unlock := cfg.LockUpdate()
	defer unlock()
	fileCfg, err := cfg.LoadFile()
	if err != nil {
		return err
	}
	if fileCfg.CommandConfig == nil {
		fileCfg.CommandConfig = map[string]cfg.CommandDefConfig{}
	}
	entry := fileCfg.CommandConfig[id]
	mutate(&entry)
	if len(entry.Env) == 0 {
		entry.Env = nil
	}
	// 空配置不留痕（表里不出现空条目）。
	if len(entry.Env) == 0 && !entry.NoSandbox {
		delete(fileCfg.CommandConfig, id)
	} else {
		fileCfg.CommandConfig[id] = entry
	}
	if err := cfg.Save(fileCfg); err != nil {
		return err
	}
	c.setCommandOptions(fileCfg.Commands, fileCfg.CommandConfig)
	return nil
}

// printCommandConfig 输出该定义当前的配置（表内容，不是生效后的 env 全集）。
func (c *Client) printCommandConfig(inv *commands.Invocation, def *hostCommand, asJSON bool) error {
	entry := c.options().CommandConfig[def.ID]
	if asJSON {
		data, err := json.Marshal(map[string]any{
			"alias":      def.Alias,
			"id":         def.ID,
			"source":     def.Source,
			"env":        entry.Env,
			"no_sandbox": entry.NoSandbox,
		})
		if err != nil {
			return commands.Exitf(inv, 1, "commands config: %s", err)
		}
		fmt.Fprintln(inv.Stdout, string(data))
		return nil
	}
	fmt.Fprintf(inv.Stdout, "%s (%s, source=%s)\n", def.Alias, def.ID, def.Source)
	keys := make([]string, 0, len(entry.Env))
	for key := range entry.Env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		fmt.Fprintf(inv.Stdout, "  %s=%s\n", key, entry.Env[key])
	}
	fmt.Fprintf(inv.Stdout, "  no_sandbox=%t\n", entry.NoSandbox)
	return nil
}
