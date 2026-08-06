package api

import (
	"strings"

	"github.com/veypi/aic-pod/libs/host"
	"github.com/veypi/vigo"
)

// StartHostReq 是 start 的请求：body 可携带临时覆盖（不持久化）。
type StartHostReq struct {
	Host       string `json:"host" src:"json"`
	Credential string `json:"credential" src:"json"`
}

// StartHost 启动 host 会话（基于当前有效配置 + 临时覆盖）。
func StartHost(x *vigo.X, req *StartHostReq) (*OKResp, error) {
	o := effective()
	if h := strings.TrimSpace(req.Host); h != "" {
		o.Host = h
	}
	if c := strings.TrimSpace(req.Credential); c != "" {
		o.Key = c
	}
	if err := host.Start(o); err != nil {
		return nil, vigo.ErrInvalidArg.WithError(err)
	}
	return &OKResp{OK: true}, nil
}

// StopHost 停止 host 会话。
func StopHost(x *vigo.X) (*OKResp, error) {
	host.Stop()
	return &OKResp{OK: true}, nil
}
