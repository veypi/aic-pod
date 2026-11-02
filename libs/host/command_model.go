package host

import (
	"fmt"
	"net"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/veypi/aic-pod/cfg"
	"github.com/veypi/aic-pod/libs/mcpx"
	skillmanifest "github.com/veypi/aic-skills/manifest"
)

const (
	commandSourceSkill  = "skill"
	commandSourceConfig = "config"
)

// commandManifest 是权威解析结果在 pod 侧的投影：UI 由平台其他层消费，
// pod 只取 Host。解析与校验统一走 aic-skills/manifest（UI 声明同契约校验）。
type commandManifest struct {
	Host map[string]manifestHost
}

type manifestHost struct {
	cfg.CommandConfig
	Streams map[string]manifestStream `json:"streams,omitempty"`
}

type manifestStream struct {
	AddressEnv string `json:"address_env"`
}

type hostStream struct {
	Name       string `json:"name"`
	AddressEnv string `json:"address_env"`
	Address    string `json:"address"`
}

type hostCommand struct {
	ID       string
	Alias    string
	Source   string
	SkillID  string
	Order    uint64
	Revision uint64
	Root     string
	Config   cfg.CommandConfig
	// Local 是设备侧命令定义级配置（command_config 表，按定义身份）：
	// 应用配置 env + 沙箱策略。声明来源（manifest / 设备 commands 段）不携带。
	Local   cfg.CommandDefConfig
	Streams map[string]hostStream
}

func (d *hostCommand) clone() *hostCommand {
	out := *d
	out.Config.Run = append([]string(nil), d.Config.Run...)
	if d.Config.Env != nil {
		out.Config.Env = make(map[string]string, len(d.Config.Env))
		for k, v := range d.Config.Env {
			out.Config.Env[k] = v
		}
	}
	if d.Local.Env != nil {
		out.Local.Env = make(map[string]string, len(d.Local.Env))
		for k, v := range d.Local.Env {
			out.Local.Env[k] = v
		}
	}
	if d.Streams != nil {
		out.Streams = make(map[string]hostStream, len(d.Streams))
		for k, v := range d.Streams {
			out.Streams[k] = v
		}
	}
	return &out
}

func (d *hostCommand) kind() string {
	if len(d.Streams) > 0 && d.Config.Protocol == "" {
		return "stream"
	}
	if d.Config.Protocol == "mcp" {
		return "mcp"
	}
	return "process"
}

func (d *hostCommand) resident() bool {
	return d.Config.Protocol == "mcp" || len(d.Streams) > 0
}

func (d *hostCommand) cwd() string {
	if d.Source == commandSourceSkill {
		return d.Root
	}
	return d.Config.Cwd
}

func (d *hostCommand) noSandbox() bool {
	return d.Config.NoSandbox || d.Local.NoSandbox
}

func (d *hostCommand) environment() map[string]string {
	env := make(map[string]string, len(d.Config.Env)+len(d.Local.Env)+len(d.Streams))
	for k, v := range d.Config.Env {
		env[k] = v
	}
	// 设备侧配置覆盖声明来源的 env（同一张表的应用配置部分）。
	for k, v := range d.Local.Env {
		env[k] = v
	}
	for _, stream := range d.Streams {
		env[stream.AddressEnv] = stream.Address
	}
	return env
}

func (d *hostCommand) mcpConfig() mcpx.Config {
	cfg := mcpx.Config{
		URL:         d.Config.URL,
		IdleTimeout: d.Config.IdleTimeout,
		Env:         d.environment(),
		NoSandbox:   d.noSandbox(),
	}
	if len(d.Config.Run) > 0 {
		cfg.Command = d.Config.Run[0]
		cfg.Args = append([]string(nil), d.Config.Run[1:]...)
		cfg.Cwd = d.cwd()
	}
	return cfg
}

var addressEnvPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func parseCommandManifest(data []byte) (*commandManifest, error) {
	parsed, err := skillmanifest.Parse(data)
	if err != nil {
		return nil, err
	}
	out := &commandManifest{Host: make(map[string]manifestHost, len(parsed.Host))}
	for alias, service := range parsed.Host {
		host := manifestHost{CommandConfig: cfg.CommandConfig{
			Run: service.Run, URL: service.URL, Protocol: service.Protocol, IdleTimeout: service.IdleTimeout,
		}}
		if len(service.Streams) > 0 {
			host.Streams = make(map[string]manifestStream, len(service.Streams))
			for name, stream := range service.Streams {
				host.Streams[name] = manifestStream{AddressEnv: stream.AddressEnv}
			}
		}
		out.Host[alias] = host
	}
	return out, nil
}

func skillCommands(skillID, root string, order uint64, m *commandManifest) (map[string]*hostCommand, error) {
	defs := make(map[string]*hostCommand, len(m.Host))
	for alias, host := range m.Host {
		config := host.CommandConfig
		if err := mcpx.ValidateAlias(alias); err != nil {
			return nil, err
		}
		if config.Cwd != "" || len(config.Env) > 0 || config.NoSandbox {
			return nil, fmt.Errorf("skill command %s: cwd/env/no_sandbox are device-owner settings", alias)
		}
		if len(config.Run) > 0 && !filepath.IsAbs(config.Run[0]) && strings.ContainsAny(config.Run[0], "/\\") {
			config.Run = append([]string(nil), config.Run...)
			config.Run[0] = filepath.Join(root, filepath.FromSlash(config.Run[0]))
		}
		id := "skill:" + skillID + ":" + alias
		def := &hostCommand{
			ID: id, Alias: alias, Source: commandSourceSkill, SkillID: skillID,
			Order: order, Root: root, Config: config,
		}
		for name, stream := range host.Streams {
			if !addressEnvPattern.MatchString(stream.AddressEnv) {
				return nil, fmt.Errorf("stream %s: invalid address_env %q", name, stream.AddressEnv)
			}
			if !addressEnvPattern.MatchString(name) {
				return nil, fmt.Errorf("stream name %q is invalid", name)
			}
			if len(config.Run) == 0 {
				return nil, fmt.Errorf("stream %s: command %q must use run", name, alias)
			}
			if def.Streams == nil {
				def.Streams = map[string]hostStream{}
			}
			def.Streams[name] = hostStream{Name: name, AddressEnv: stream.AddressEnv}
		}
		defs[id] = def
	}
	for _, def := range defs {
		addressOwners := map[string]string{}
		for name, stream := range def.Streams {
			if previous, exists := addressOwners[stream.AddressEnv]; exists {
				return nil, fmt.Errorf("command %s: streams %s and %s share address_env %q", def.Alias, previous, name, stream.AddressEnv)
			}
			addressOwners[stream.AddressEnv] = name
		}
		if err := validateHostCommand(def); err != nil {
			return nil, err
		}
	}
	return defs, nil
}

func validateHostCommand(d *hostCommand) error {
	if err := mcpx.ValidateAlias(d.Alias); err != nil {
		return err
	}
	if (len(d.Config.Run) == 0) == (d.Config.URL == "") {
		return fmt.Errorf("command %s: configure exactly one of run or url", d.Alias)
	}
	if len(d.Config.Run) > 0 && strings.TrimSpace(d.Config.Run[0]) == "" {
		return fmt.Errorf("command %s: run program is empty", d.Alias)
	}
	if d.Config.Protocol != "" && d.Config.Protocol != "mcp" {
		return fmt.Errorf("command %s: unsupported protocol %q", d.Alias, d.Config.Protocol)
	}
	if d.Config.URL != "" && d.Config.Protocol != "mcp" {
		return fmt.Errorf("command %s: url requires protocol=mcp", d.Alias)
	}
	if d.resident() {
		if _, err := d.mcpConfig().IdleDuration(); err != nil {
			return fmt.Errorf("command %s: invalid idle_timeout: %w", d.Alias, err)
		}
	} else if d.Config.IdleTimeout != "" {
		return fmt.Errorf("command %s: idle_timeout requires a resident command", d.Alias)
	}
	if d.Config.Protocol == "mcp" {
		if err := d.mcpConfig().Validate(d.Alias); err != nil {
			return err
		}
	}
	if d.Config.URL != "" {
		// 远程端点没有进程：cwd/env 只对 run 型命令有意义，不接受静默丢弃。
		// （远程 MCP 的 header/token 注入是下一阶段的能力。）
		if d.Config.Cwd != "" || len(d.Config.Env) > 0 {
			return fmt.Errorf("command %s: cwd/env require a run-based command", d.Alias)
		}
		u, err := url.Parse(d.Config.URL)
		if err != nil || u.Host == "" || u.User != nil || u.Fragment != "" ||
			(u.Scheme != "http" && u.Scheme != "https") {
			return fmt.Errorf("command %s: MCP endpoint must be a plain http(s) URL without userinfo or fragment", d.Alias)
		}
	}
	if d.cwd() != "" && !filepath.IsAbs(d.cwd()) {
		return fmt.Errorf("command %s: cwd must be absolute", d.Alias)
	}
	for key, value := range d.environment() {
		if key == "" || strings.ContainsAny(key, "=\x00") || strings.ContainsRune(value, 0) {
			return fmt.Errorf("command %s: invalid environment", d.Alias)
		}
	}
	return nil
}

func allocateCommandStreams(d *hostCommand) error {
	for name, stream := range d.Streams {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return fmt.Errorf("allocate stream %s: %w", name, err)
		}
		stream.Address = listener.Addr().String()
		if err := listener.Close(); err != nil {
			return fmt.Errorf("allocate stream %s: %w", name, err)
		}
		d.Streams[name] = stream
	}
	return nil
}
