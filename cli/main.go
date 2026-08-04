// AIC CLI — 部署在 PC (Windows/macOS/Linux) 上的 host agent，通过 NATS 连接 AIC 平台。
//
// 子命令：
//
//	aic run      连接运行（默认子命令；`aic -key xxx` 等价于 `aic run -key xxx`）
//	aic bind     写入绑定凭证（保存到配置文件，desktop 共享同一份）
//	aic config   查看/修改配置
//	aic version  显示版本
//
// 配置解析链：显式 flag > AIC_* 环境变量 > 配置文件 > 默认值。
// 配置文件：os.UserConfigDir()/aic/config.json（与 desktop 共享）。
// 环境变量：AIC_HOST / AIC_KEY / AIC_DIR / AIC_EXEC_TIMEOUT。
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/veypi/aic-pod/sdk/host"
)

var version = "v0.5.1"

func main() {
	args := os.Args[1:]
	// 兼容旧习惯：首参数是 flag 则视为 run（`aic -key xxx` → `aic run -key xxx`）
	if len(args) > 0 && strings.HasPrefix(args[0], "-") {
		args = append([]string{"run"}, args...)
	}
	cmd := "run"
	if len(args) > 0 {
		cmd = args[0]
		args = args[1:]
	}
	switch cmd {
	case "run":
		runCmd(args)
	case "bind":
		bindCmd(args)
	case "config":
		configCmd(args)
	case "version":
		fmt.Printf("aic %s\n", version)
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", cmd)
		usage()
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `Usage: aic <command> [options]

Commands:
  run       Connect and serve (default; flags without command also work)
  bind      Save credential to config file
  config    Show or edit config (list / get <key> / set <key> <value>)
  version   Show version

Run "aic <command> -h" for command options.

Config chain: flags > AIC_* env > config file > defaults
  config file: `+configPathHint()+`
  env: AIC_HOST AIC_KEY AIC_DIR AIC_EXEC_TIMEOUT`)
}

func configPathHint() string {
	if p, err := host.ConfigPath(); err == nil {
		return p
	}
	return "~/.config/aic/config.json"
}

// addCommonFlags 注册 run/bind 共享的配置覆盖 flag（默认值 = 当前解析出的配置）。
func addCommonFlags(fs *flag.FlagSet, cfg *host.Config) {
	fs.StringVar(&cfg.Host, "host", cfg.Host, "Platform address")
	fs.StringVar(&cfg.Credential, "key", cfg.Credential, "Credential key (from the platform's device page)")
	fs.StringVar(&cfg.WorkDir, "dir", cfg.WorkDir, "Working directory for exec (default: system temp dir)")
	fs.StringVar(&cfg.ExecTimeout, "exec-timeout", cfg.ExecTimeout, "Exec background timeout (default: 30m)")
}

// loadWithFlags 配置解析链：配置文件 + AIC_* env + 显式 flag。
// flag 默认值绑在配置指针上：Parse 只覆盖显式给出的 flag，未设置的字段保持配置原值。
func loadWithFlags(fs *flag.FlagSet, args []string) (host.Config, error) {
	cfg, err := host.Load()
	if err != nil {
		return cfg, err
	}
	addCommonFlags(fs, &cfg)
	if err := fs.Parse(args); err != nil {
		return cfg, err
	}
	return cfg.Normalize(), nil
}

func newFlagSet(name string) *flag.FlagSet {
	return flag.NewFlagSet(name, flag.ExitOnError)
}

func runCmd(args []string) {
	fs := newFlagSet("run")
	cfg, err := loadWithFlags(fs, args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
		os.Exit(1)
	}
	if strings.TrimSpace(cfg.Credential) == "" {
		fmt.Fprintln(os.Stderr, "credential is required: aic run -key <key>")
		fmt.Fprintln(os.Stderr, "or bind it first:   aic bind <key>")
		os.Exit(1)
	}
	opts, err := cfg.Options("cli", version, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
		os.Exit(1)
	}
	client, err := host.Connect(opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "connect: %v\n", err)
		os.Exit(1)
	}

	// 阻塞等待 SIGINT/SIGTERM
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	fmt.Println("shutting down...")
	client.Close()
}

func bindCmd(args []string) {
	// 凭证可作为首个位置参数：aic bind <credential> [-host x]
	//（Go flag 包遇首个非 flag 参数停止解析，先剥离位置参数再 Parse）
	var positional string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		positional = args[0]
		args = args[1:]
	}
	fs := newFlagSet("bind")
	cfg, err := loadWithFlags(fs, args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
		os.Exit(1)
	}
	if positional != "" {
		cfg.Credential = positional
	}
	if strings.TrimSpace(cfg.Credential) == "" {
		fmt.Fprintln(os.Stderr, "usage: aic bind <key> [-host https://ivec.ai]")
		os.Exit(1)
	}
	if err := host.SaveConfig(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "save: %v\n", err)
		os.Exit(1)
	}
	p, _ := host.ConfigPath()
	fmt.Printf("saved to %s (host=%s)\n", p, cfg.Host)
}

// configKeys 是 config 子命令支持的键（含别名 → 字段）。
var configKeys = map[string]func(*host.Config) *string{
	"host":         func(c *host.Config) *string { return &c.Host },
	"key":          func(c *host.Config) *string { return &c.Credential },
	"credential":   func(c *host.Config) *string { return &c.Credential },
	"dir":          func(c *host.Config) *string { return &c.WorkDir },
	"work_dir":     func(c *host.Config) *string { return &c.WorkDir },
	"exec_timeout": func(c *host.Config) *string { return &c.ExecTimeout },
}

func configCmd(args []string) {
	sub := "list"
	if len(args) > 0 {
		sub = args[0]
		args = args[1:]
	}
	cfg, err := host.LoadConfig() // 只展示/修改文件内容，不叠加 env
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
		os.Exit(1)
	}
	switch sub {
	case "list":
		data, _ := json.MarshalIndent(cfg, "", "  ")
		fmt.Println(string(data))
	case "get":
		if len(args) != 1 {
			fmt.Fprintln(os.Stderr, "usage: aic config get <key>")
			os.Exit(1)
		}
		field, ok := configKeys[args[0]]
		if !ok {
			fmt.Fprintf(os.Stderr, "unknown key %q (host/key/dir/exec_timeout)\n", args[0])
			os.Exit(1)
		}
		fmt.Println(*field(&cfg))
	case "set":
		if len(args) != 2 {
			fmt.Fprintln(os.Stderr, "usage: aic config set <key> <value>")
			os.Exit(1)
		}
		field, ok := configKeys[args[0]]
		if !ok {
			fmt.Fprintf(os.Stderr, "unknown key %q (host/key/dir/exec_timeout)\n", args[0])
			os.Exit(1)
		}
		*field(&cfg) = args[1]
		if err := host.SaveConfig(cfg); err != nil {
			fmt.Fprintf(os.Stderr, "save: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("%s = %s\n", args[0], args[1])
	default:
		fmt.Fprintf(os.Stderr, "unknown config subcommand %q (list/get/set)\n", sub)
		os.Exit(1)
	}
}
