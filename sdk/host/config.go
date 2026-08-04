package host

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/veypi/vigo/flags"
)

// Config 是 cli 与 desktop 共享的唯一配置模型（配置参数就是一个结构体，
// vigo/flags AutoRegister/LoadCfg/DumpCfg 直接使用）：
//
//   - json tag：flag 名（-host/-key/-work_dir/-exec_timeout）与 env 键
//     （HOST/KEY/WORK_DIR/EXEC_TIMEOUT）的来源，也是配置文件的键
//   - default tag：结构体默认值（无文件无 env 无 flag 时生效）
//   - desc tag：-h 帮助文案
//
// 落盘位置：os.UserConfigDir()/aic/config.yaml（flags.DumpCfg，原子写），
// cli 与 desktop 读写同一份——任一端的修改（编辑文件 / 页面绑定）另一端启动即生效。
//
// 解析优先级：显式 flag > 环境变量 > 配置文件（flags.LoadCfg）> default tag
type Config struct {
	Host        string `json:"host" default:"https://ivec.ai" desc:"platform address (NATS endpoint inferred)"`
	Key         string `json:"key" desc:"binding credential key (from platform device page)"`
	WorkDir     string `json:"work_dir" desc:"working directory for exec (default: system temp dir)"`
	ExecTimeout string `json:"exec_timeout" default:"30m" desc:"exec background timeout"`
}

// DefaultConfig 返回默认配置。
func DefaultConfig() Config {
	return Config{Host: DefaultHost, ExecTimeout: "30m"}
}

// Normalize 填充缺省值（Host 空 → DefaultHost）。
func (cfg Config) Normalize() Config {
	if strings.TrimSpace(cfg.Host) == "" {
		cfg.Host = DefaultHost
	}
	return cfg
}

// ConfigPath 返回配置文件路径：UserConfigDir/aic/config.yaml。
func ConfigPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "aic", "config.yaml"), nil
}

// LoadConfig 读取配置文件（yaml，flags.LoadCfg）；文件不存在返回默认配置（非错误），
// 损坏文件由 flags 记 warn 并返回当前值，不让坏文件阻断启动。
func LoadConfig() (Config, error) {
	p, err := ConfigPath()
	if err != nil {
		return DefaultConfig(), err
	}
	cfg := DefaultConfig()
	flags.LoadCfg(p, &cfg)
	return cfg.Normalize(), nil
}

// SaveConfig 持久化配置（yaml，flags.DumpCfg 原子写；含凭证，文件权限 0600）。
func SaveConfig(cfg Config) error {
	p, err := ConfigPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	if err := flags.DumpCfg(p, cfg.Normalize()); err != nil {
		return err
	}
	// DumpCfg 以 0644 创建，凭证敏感改 0600
	return os.Chmod(p, 0o600)
}

// EnvOverlay 与 Load 已随配置解析迁移到 vigo/flags AutoRegister（其 getDefaultValue
// 优先读环境变量 HOST/CREDENTIAL/WORK_DIR/EXEC_TIMEOUT，再叠加显式 flag），不再自建。

// Options 将配置转换为 host 客户端 Options（解析 ExecTimeout）。
// deviceType 由调用方指定（cli/desktop），version 为客户端版本（va.b.c）。
func (cfg Config) Options(deviceType, version string, onLog func(string, ...any)) (Options, error) {
	timeout := 30 * time.Minute
	if s := strings.TrimSpace(cfg.ExecTimeout); s != "" {
		d, err := time.ParseDuration(s)
		if err != nil {
			return Options{}, fmt.Errorf("invalid exec_timeout %q: %w", s, err)
		}
		timeout = d
	}
	return Options{
		Host:        cfg.Host,
		Key:         cfg.Key,
		WorkDir:     cfg.WorkDir,
		DeviceType:  deviceType,
		Version:     version,
		ExecTimeout: timeout,
		OnLog:       onLog,
	}, nil
}
