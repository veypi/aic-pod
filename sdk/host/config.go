package host

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Config 是 cli 与 desktop 共享的唯一配置模型。
//
// 落盘位置：os.UserConfigDir()/aic/config.json（0600），两端读写同一份——
// cli 的 `aic bind` 配好后 desktop 启动即生效，反之亦然。
//
// 解析优先级（Resolve 链，高到低）：
//
//	显式参数（cli flag / 调用方覆盖） > AIC_* 环境变量 > 配置文件 > 默认值
//
// 环境变量统一 AIC_ 前缀（docker 经 --env-file/-e 注入，进程内不读 .env）：
//
//	AIC_HOST          平台地址
//	AIC_KEY           绑定凭证
//	AIC_DIR           exec/fs 缺省工作区
//	AIC_NAME          设备展示名称
//	AIC_NATS_URL      显式 NATS 端点（空 = 按 Host 推断）
//	AIC_EXEC_TIMEOUT  exec 后台超时（如 30m）
type Config struct {
	Host        string `json:"host"`                   // 平台地址（默认 DefaultHost）
	Credential  string `json:"credential"`             // 绑定凭证 <host_id>.<cred_ver>.<secret>.<uid>
	WorkDir     string `json:"work_dir,omitempty"`     // exec/fs 缺省工作区（默认系统临时目录）
	DeviceName  string `json:"device_name,omitempty"`  // 展示名称（默认 hostname）
	NATSURL     string `json:"nats_url,omitempty"`     // 显式 NATS 端点（空 = 按 Host 推断）
	ExecTimeout string `json:"exec_timeout,omitempty"` // exec 后台超时（默认 30m）
}

// DefaultConfig 返回默认配置。
func DefaultConfig() Config {
	return Config{Host: DefaultHost}
}

// Normalize 填充缺省值（Host 空 → DefaultHost）。
func (cfg Config) Normalize() Config {
	if strings.TrimSpace(cfg.Host) == "" {
		cfg.Host = DefaultHost
	}
	return cfg
}

// ConfigPath 返回配置文件路径：UserConfigDir/aic/config.json。
func ConfigPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "aic", "config.json"), nil
}

// LoadConfig 读取配置文件；文件不存在返回默认配置（非错误）。
func LoadConfig() (Config, error) {
	p, err := ConfigPath()
	if err != nil {
		return DefaultConfig(), err
	}
	data, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return DefaultConfig(), nil
		}
		return DefaultConfig(), err
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return DefaultConfig(), fmt.Errorf("parse %s: %w", p, err)
	}
	return cfg.Normalize(), nil
}

// SaveConfig 持久化配置（0600，父目录自动创建）。
func SaveConfig(cfg Config) error {
	p, err := ConfigPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg.Normalize(), "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, data, 0o600)
}

// EnvOverlay 将 AIC_* 环境变量覆盖到 cfg 上（不持久化），返回新配置。
func EnvOverlay(cfg Config) Config {
	if v := strings.TrimSpace(os.Getenv("AIC_HOST")); v != "" {
		cfg.Host = v
	}
	if v := strings.TrimSpace(os.Getenv("AIC_KEY")); v != "" {
		cfg.Credential = v
	}
	if v := strings.TrimSpace(os.Getenv("AIC_DIR")); v != "" {
		cfg.WorkDir = v
	}
	if v := strings.TrimSpace(os.Getenv("AIC_NAME")); v != "" {
		cfg.DeviceName = v
	}
	if v := strings.TrimSpace(os.Getenv("AIC_NATS_URL")); v != "" {
		cfg.NATSURL = v
	}
	if v := strings.TrimSpace(os.Getenv("AIC_EXEC_TIMEOUT")); v != "" {
		cfg.ExecTimeout = v
	}
	return cfg.Normalize()
}

// Load 是配置解析链的便捷入口：配置文件 + AIC_* 环境变量覆盖。
// 调用方的显式参数在此结果上再做最后一层覆盖。
func Load() (Config, error) {
	cfg, err := LoadConfig()
	if err != nil {
		return cfg, err
	}
	return EnvOverlay(cfg), nil
}

// Options 将配置转换为 host 客户端 Options（解析 ExecTimeout、推断 NATSURL）。
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
		Credential:  cfg.Credential,
		WorkDir:     cfg.WorkDir,
		DeviceName:  cfg.DeviceName,
		DeviceType:  deviceType,
		Version:     version,
		NATSURL:     cfg.NATSURL,
		ExecTimeout: timeout,
		OnLog:       onLog,
	}, nil
}
