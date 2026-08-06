// Copyright (C) 2025 veypi <i@veypi.com>
// Distributed under terms of the MIT license.

// Package cfg 是 aic-pod 的配置中心（cli/desktop 共用同一份配置）：
// Options 结构体 + Global 全局有效配置，落盘 UserConfigDir/aic/config.yaml。
package cfg

import (
	"crypto/rand"
	"encoding/hex"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/rs/zerolog"
	"github.com/veypi/vigo/flags"
	"gopkg.in/natefinch/lumberjack.v2"
)

// DefaultHost 是默认平台地址。
const DefaultHost = "https://ivec.ai"

// Version 客户端版本：Makefile -X github.com/veypi/aic-pod/cfg.Version 注入 git
// 版本，未注入时以此兑底。发版只改本变量与 browser/manifest.json（无 v 前缀）。
var Version = "v0.5.3"

// DeviceType 客户端类型（cli/desktop），启动时固定（desktop main 覆盖为 "desktop"）。
var DeviceType = "cli"

// Options 是 cli 与 desktop 共享的唯一配置模型（配置参数就是一个结构体，
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
type Options struct {
	Host        string `json:"host" default:"https://ivec.ai" desc:"platform address (NATS endpoint inferred)"`
	Key         string `json:"key" desc:"binding credential key (from platform device page)"`
	WorkDir     string `json:"work_dir" desc:"working directory for exec (default: system temp dir)"`
	ExecTimeout string `json:"exec_timeout" default:"30m" desc:"exec background timeout"`

	// 进程级运行时态（unexported，不参与序列化/落盘）：
	port int    // 本地管理 API 监听端口（api.Start 监听后 SetPort 写入）
	code string // local_code 校验码（NewOptions 随机生成，生命周期 = 进程）
}

// Port 返回本地管理 API 监听端口（未启动为 0）。
func (o *Options) Port() int { return o.port }

// SetPort 写入本地管理 API 实际监听端口（仅 api.Start 调用）。
func (o *Options) SetPort(p int) { o.port = p }

// Code 返回 local_code 校验码（x-aic-code 头，不含端口前缀）。
func (o *Options) Code() string { return o.code }

// newCode 生成 local_code 校验码（32 hex）。
func newCode() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "" // crypto/rand 不可用时留空（所有 API 调用 401，安全侧失败）
	}
	return hex.EncodeToString(buf)
}

// Global 全局有效配置：NewOptions 初始化（code 随即生成）→ Load 填充文件值 →
// flags.AutoRegister(Global) 叠加 flag/env；本地 API 的写操作（bind/set_config）
// 同步更新 Global 并 Save 落盘。
var Global = NewOptions()

// NewOptions 返回带默认值的配置实例（code 随机生成，进程级）。
func NewOptions() *Options {
	return &Options{Host: DefaultHost, ExecTimeout: "30m", code: newCode()}
}

// Normalize 填充缺省值（Host 空 → DefaultHost）。
func (o *Options) Normalize() {
	if strings.TrimSpace(o.Host) == "" {
		o.Host = DefaultHost
	}
}

// HostsURL 由平台地址推导设备管理页入口 {host}/hosts（host 可带产品壳路径前缀，
// 如 http://127.0.0.1:4000/rses/aiv → http://127.0.0.1:4000/rses/aiv/hosts）。
// local_code 由调用方拼接（?local_code={port}.{code}）。
func (o *Options) HostsURL() string {
	h := strings.TrimSpace(o.Host)
	if h == "" {
		h = DefaultHost
	}
	if !strings.Contains(h, "://") {
		h = "https://" + h
	}
	u, err := url.Parse(h)
	if err != nil || u.Host == "" {
		return h
	}
	p := strings.TrimSuffix(u.Path, "/")
	if !strings.HasSuffix(p, "/hosts") {
		p += "/hosts"
	}
	u.Path = p
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}

// Path 返回配置文件路径：UserConfigDir/aic/config.yaml。
func Path() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "aic", "config.yaml"), nil
}

// LogPath 返回日志文件路径：UserConfigDir/aic/aic.log（get_log 的数据源）。
func LogPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "aic", "aic.log"), nil
}

// LogWriter 返回日志文件 writer（console 格式无色，lumberjack 滚动 16MB×3）。
// cli 与 ConsoleWriter 双写；desktop 仅文件。
func LogWriter() (io.Writer, error) {
	p, err := LogPath()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return nil, err
	}
	w := zerolog.NewConsoleWriter()
	w.Out = &lumberjack.Logger{Filename: p, MaxSize: 16, MaxBackups: 3, LocalTime: true}
	w.NoColor = true
	return w, nil
}

// LoadFile 仅读取配置文件返回独立副本（yaml，flags.LoadCfg），不触碰 Global——
// 页面写操作（bind/set_config）落盘用：基于文件配置修改，flag/env 启动覆盖不落盘。
// 文件不存在返回默认配置（非错误），损坏文件由 flags 记 warn 并返回当前值。
func LoadFile() (*Options, error) {
	o := NewOptions()
	p, err := Path()
	if err != nil {
		return o, err
	}
	flags.LoadCfg(p, o)
	o.Normalize()
	return o, nil
}

// Load 读取配置文件填充 Global 并返回（损坏文件不阻断启动，见 LoadFile）。
func Load() (*Options, error) {
	o, err := LoadFile()
	Global = o
	return o, err
}

// Save 持久化配置（yaml，flags.DumpCfg 原子写；含凭证，文件权限 0600）。
func Save(o *Options) error {
	p, err := Path()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	o.Normalize()
	if err := flags.DumpCfg(p, *o); err != nil {
		return err
	}
	// DumpCfg 以 0644 创建，凭证敏感改 0600
	return os.Chmod(p, 0o600)
}
