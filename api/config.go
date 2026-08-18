package api

import (
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/veypi/aic-pod/cfg"
	"github.com/veypi/aic-pod/libs/host"
	"github.com/veypi/vigo"
	"github.com/veypi/vigo/logv"
)

// configView 是 get_config 的返回视图（含 key——设置窗口需显示当前凭证；
// 本地 API 受 code 校验保护）。
type configView struct {
	Host        string `json:"host"`
	Key         string `json:"key"`
	WorkDir     string `json:"work_dir"`
	ExecTimeout string `json:"exec_timeout"`
	HomePath    string `json:"home_path"`
	NoSandbox   bool   `json:"no_sandbox"`
}

// GetConfig 返回当前有效配置（cfg.Global：启动解析值 + 页面写操作同步）。
func GetConfig(x *vigo.X) (*configView, error) {
	o := effective()
	return &configView{Host: o.Host, Key: o.Key, WorkDir: o.WorkDir, ExecTimeout: o.ExecTimeout, HomePath: o.NormalizedHomePath(), NoSandbox: o.NoSandbox}, nil
}

// SetConfigReq 是 set_config 的白名单参数（host/work_dir/exec_timeout/home_path/no_sandbox
// 可写；key 不走 set_config——只走 Bind，body 中的 credential 不得被持久化）。
type SetConfigReq struct {
	Host        string `json:"host" src:"json"`
	WorkDir     string `json:"work_dir" src:"json"`
	ExecTimeout string `json:"exec_timeout" src:"json"`
	HomePath    string `json:"home_path" src:"json"`
	NoSandbox   bool   `json:"no_sandbox" src:"json"`
}

// SetConfig 持久化运行参数并应用：基于文件配置落盘（flag/env 覆盖不落盘），
// 内存态同步 cfg.Global；host/work_dir/exec_timeout 变更经 ApplyConfig 应用
// （保留会话与 bg 任务，NATS 地址变化时重连）。
func SetConfig(x *vigo.X, req *SetConfigReq) (*OKResp, error) {
	if s := strings.TrimSpace(req.ExecTimeout); s != "" {
		if _, err := time.ParseDuration(s); err != nil {
			return nil, vigo.ErrInvalidArg.WithString("invalid exec_timeout: " + err.Error())
		}
	}
	// 持久化运行参数（基于文件配置）
	fileCfg, err := cfg.LoadFile()
	if err != nil {
		return nil, vigo.ErrInternalServer.WithError(err)
	}
	if h := strings.TrimSpace(req.Host); h != "" {
		fileCfg.Host = h
	}
	// work_dir：~ 展开 + 归一绝对路径 + 有效性校验，落盘即真实路径
	//（Go exec 不做 shell 展开，配置页填 ~/test 会因路径不存在导致所有 exec 失败）
	wd := strings.TrimSpace(req.WorkDir)
	if wd != "" {
		wd = expandHome(wd)
		abs, err := filepath.Abs(wd)
		if err != nil {
			return nil, vigo.ErrInvalidArg.WithString("invalid work_dir: " + err.Error())
		}
		st, err := os.Stat(abs)
		if err != nil || !st.IsDir() {
			return nil, vigo.ErrInvalidArg.WithString("invalid work_dir: not a directory: " + abs)
		}
		wd = abs
	}
	fileCfg.WorkDir = wd
	fileCfg.ExecTimeout = strings.TrimSpace(req.ExecTimeout)
	fileCfg.NoSandbox = req.NoSandbox
	// home_path：必须以单个 / 开头（// 开头是协议相对 URL，拼接后会跳转到别的站点，拒绝）
	if hp := strings.TrimSpace(req.HomePath); hp != "" {
		if !strings.HasPrefix(hp, "/") || strings.HasPrefix(hp, "//") {
			return nil, vigo.ErrInvalidArg.WithString("invalid home_path: must be a path starting with / (e.g. / or /a)")
		}
		fileCfg.HomePath = hp
	} else {
		fileCfg.HomePath = "/" // 清空 = 恢复默认首页
	}
	if err := cfg.Save(fileCfg); err != nil {
		return nil, vigo.ErrInternalServer.WithError(err)
	}
	mu.Lock()
	hostChanged := cfg.Global.Host != fileCfg.Host
	workDirChanged := cfg.Global.WorkDir != fileCfg.WorkDir
	execTimeoutChanged := cfg.Global.ExecTimeout != fileCfg.ExecTimeout
	noSandboxChanged := cfg.Global.NoSandbox != fileCfg.NoSandbox
	cfg.Global.Host = fileCfg.Host
	cfg.Global.WorkDir = fileCfg.WorkDir
	cfg.Global.ExecTimeout = fileCfg.ExecTimeout
	cfg.Global.HomePath = fileCfg.HomePath
	cfg.Global.NoSandbox = fileCfg.NoSandbox
	o := *cfg.Global
	mu.Unlock()
	// 运行参数变更（host/work_dir/exec_timeout/no_sandbox）：应用新配置——保留会话与
	// bg 任务，仅更新参数；NATS 地址变化时重连（Client.Reconfigure）
	if host.Running() && (hostChanged || workDirChanged || execTimeoutChanged || noSandboxChanged) {
		if err := host.ApplyConfig(o); err != nil {
			logv.Warn().Msgf("apply config failed: %v", err)
		}
	}
	return &OKResp{OK: true}, nil
}

// expandHome 展开 work_dir 的 ~ 前缀（~ 或 ~/xxx → 用户主目录）。
// 配置保存时调用：落盘即为真实绝对路径，运行时不再需要 shell 展开语义。
func expandHome(p string) string {
	if p == "~" {
		if h, err := os.UserHomeDir(); err == nil {
			return h
		}
		return p
	}
	if strings.HasPrefix(p, "~/") {
		if h, err := os.UserHomeDir(); err == nil {
			return filepath.Join(h, strings.TrimPrefix(p, "~/"))
		}
	}
	return p
}
