package api

import (
	"strings"

	"github.com/veypi/aic-pod/cfg"
	"github.com/veypi/aic-pod/libs/host"
	"github.com/veypi/vigo"
)

// BindReq 是 bind 的请求：只收 credential（host 由启动参数决定）。
type BindReq struct {
	Credential string `json:"credential" src:"json"`
}

// BindResp 返回绑定结果与文件配置中的平台地址。
type BindResp struct {
	OK   bool   `json:"ok"`
	Host string `json:"host"`
}

// Bind 绑定设备：保存凭证并自动启动 host 会话。
func Bind(x *vigo.X, req *BindReq) (*BindResp, error) {
	cred := strings.TrimSpace(req.Credential)
	if cred == "" {
		return nil, vigo.ErrInvalidArg.WithString("credential is empty")
	}
	// 持久化凭证（基于文件配置，flag/env 覆盖不落盘）
	fileCfg, err := cfg.LoadFile()
	if err != nil {
		return nil, vigo.ErrInternalServer.WithError(err)
	}
	fileCfg.Key = cred
	if err := cfg.Save(fileCfg); err != nil {
		return nil, vigo.ErrInternalServer.WithError(err)
	}
	// 内存态同步：启动覆盖保留 + 新凭证
	mu.Lock()
	oldKey := cfg.Global.Key
	cfg.Global.Key = cred
	startCfg := *cfg.Global
	mu.Unlock()
	// 已运行时的 bind 语义（2026-08-06 修复）：desktop 启动时 autoConnect 已
	// StartHost，重复 bind 会误报 host already running 且阻断后续 set_config。
	// 凭证未变 → 保持运行（幂等）；凭证变了（换绑）→ 重启 host 用新凭证重连。
	if host.Running() {
		if oldKey != startCfg.Key {
			host.Stop()
			if err := host.Start(startCfg); err != nil {
				return nil, vigo.ErrInvalidArg.WithError(err)
			}
		}
	} else if err := host.Start(startCfg); err != nil {
		return nil, vigo.ErrInvalidArg.WithError(err)
	}
	return &BindResp{OK: true, Host: fileCfg.Host}, nil
}

// Unbind 解绑设备：停止会话并清除凭证（保留 host/运行参数）。
func Unbind(x *vigo.X) (*OKResp, error) {
	host.Stop()
	fileCfg, err := cfg.LoadFile()
	if err != nil {
		return nil, vigo.ErrInternalServer.WithError(err)
	}
	fileCfg.Key = ""
	if err := cfg.Save(fileCfg); err != nil {
		return nil, vigo.ErrInternalServer.WithError(err)
	}
	mu.Lock()
	cfg.Global.Key = ""
	mu.Unlock()
	return &OKResp{OK: true}, nil
}

// boundHostID 从凭证首段解析 host_id（与连接状态无关）。
func boundHostID(credential string) string {
	parts := strings.Split(strings.TrimSpace(credential), ".")
	if len(parts) == 4 {
		return parts[0]
	}
	return ""
}
