package api

import (
	"fmt"
	"net"
	"strings"

	"github.com/veypi/aic-pod/libs/host"
	"github.com/veypi/aic-pod/libs/vcore"
	"github.com/veypi/vigo"
)

// 壳 provider 注册（§5.1 第三类命令来源）：壳进程（desktop Electron main）把
// 自身能力注册进 host 命令空间。仅白名单内的命令名可注册（声明元数据从 vcore
// 取，壳侧只提供执行通道）——防止本地通道冒名声明任意命令名。

// providerRegisterReq 是 provider/register 的请求。
type providerRegisterReq struct {
	Command string `json:"command" src:"json"` // 目前仅 "browser"
	Addr    string `json:"addr" src:"json"`    // 壳本地通道 127.0.0.1:port
	Token   string `json:"token" src:"json"`   // 壳生成，每请求回带校验
}

// ProviderRegister 注册壳命令：命令表白名单校验 → 通道地址校验（环回 TCP）→
// 注册进 host（存活则即时入表 + 重发 caps）。
func ProviderRegister(x *vigo.X, req *providerRegisterReq) (*OKResp, error) {
	name := strings.TrimSpace(req.Command)
	if name != "browser" {
		return nil, vigo.ErrInvalidArg.WithMessage(fmt.Sprintf("unsupported provider command %q (supported: browser)", name))
	}
	addr := strings.TrimSpace(req.Addr)
	hostAddr, port, err := net.SplitHostPort(addr)
	if err != nil || hostAddr != "127.0.0.1" || port == "" {
		return nil, vigo.ErrInvalidArg.WithMessage("addr must be 127.0.0.1:port")
	}
	if strings.TrimSpace(req.Token) == "" {
		return nil, vigo.ErrInvalidArg.WithMessage("token is required")
	}
	decl, ok := vcore.Decl(name)
	if !ok {
		return nil, vigo.ErrInvalidArg.WithMessage(fmt.Sprintf("no metadata for %q", name))
	}
	if err := host.RegisterProvider(host.Provider{
		Decl:       decl,
		Run:        host.RunViaShell(host.ShellChannel{Addr: addr, Token: req.Token}),
		EndSession: host.EndSessionViaShell(host.ShellChannel{Addr: addr, Token: req.Token}),
	}); err != nil {
		return nil, fmt.Errorf("register provider: %w", err)
	}
	return &OKResp{OK: true}, nil
}
