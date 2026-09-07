package host

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/veypi/aic-pod/libs/proto"
)

// 壳 provider（§5.1 统一命令声明模型的第三类命令来源）：
// 恒声明 + 启动探测之外，壳进程（desktop Electron main 等）可经本地 API
// 把自身能力（browser 等）注册进 host 命令空间——声明进 caps 重发，
// 执行经壳本地通道（127.0.0.1 TCP 换行 JSON）转发。
//
// 与探测命令的差异：provider 是进程间能力（命令的实际执行者在壳进程），
// 注册随壳进程生命周期；host 重启（cli 重进）后由壳重新注册。

// Provider 是一个壳注册命令：声明 + 执行体。
type Provider struct {
	Decl proto.CommandDecl
	Run  ProviderRunFunc
}

// ProviderRunFunc 执行一次命令调用（语义同 dispatch 的特化分支）：
// sid 绑定会话上下文；argv 为子命令参数原文（不含命令名）。
type ProviderRunFunc func(ctx context.Context, sid string, req *proto.ToolRequest, argv []string) *proto.ToolResponse

var (
	providersMu sync.RWMutex
	providers   = map[string]Provider{} // 进程级注册表（Client 生命周期独立）
)

// RegisterProvider 注册/覆盖壳命令（同名替换，壳重连场景幂等）。
// 若 host 会话存活：即时加入命令表并重发 caps（平台侧能力发现立即可见）。
func RegisterProvider(p Provider) error {
	if p.Decl.Name == "" || p.Run == nil {
		return fmt.Errorf("provider requires name and run")
	}
	providersMu.Lock()
	providers[p.Decl.Name] = p
	providersMu.Unlock()

	rtMu.Lock()
	c := rtClient
	rtMu.Unlock()
	if c != nil {
		c.addProvider(p.Decl)
		if c.nc != nil {
			c.publishCaps(c.nc)
		}
	}
	return nil
}

// lookupProvider 查询命令的 provider 注册（dispatch 路由用）。
func lookupProvider(name string) (Provider, bool) {
	providersMu.RLock()
	defer providersMu.RUnlock()
	p, ok := providers[name]
	return p, ok
}

// providerDecls 返回全部 provider 的注册声明（buildCommandTable 汇入）。
func providerDecls() []proto.CommandDecl {
	providersMu.RLock()
	defer providersMu.RUnlock()
	out := make([]proto.CommandDecl, 0, len(providers))
	for _, p := range providers {
		out = append(out, p.Decl)
	}
	return out
}

// addProvider 把 provider 声明加入存活客户端的命令表（caps 重发由调用方触发）。
func (c *Client) addProvider(decl proto.CommandDecl) {
	c.cmdsMu.Lock()
	defer c.cmdsMu.Unlock()
	if _, ok := c.cmdByName[decl.Name]; ok {
		// 覆盖：替换 cmds 中的同名条目
		for i := range c.cmds {
			if c.cmds[i].Name == decl.Name {
				c.cmds[i] = decl
				break
			}
		}
		c.cmdByName[decl.Name] = decl
		return
	}
	c.cmds = append(c.cmds, decl)
	c.cmdByName[decl.Name] = decl
}

// ---- 壳本地通道（127.0.0.1 TCP 换行 JSON） ----

// ShellChannel 是壳进程注册的本地执行通道地址 + 令牌。
// 协议：Go dial → 发送一行 JSON 请求 → 读取一行 JSON 应答 → 关闭。
type ShellChannel struct {
	Addr  string // 127.0.0.1:port（壳进程监听）
	Token string // 每请求携带，壳侧校验（防本机其他进程伪造调用）
}

// ShellRequest 是发往壳通道的请求信封（browser 等 provider 命令共用）。
type ShellRequest struct {
	Token       string   `json:"token"`
	Argv        []string `json:"argv"`
	MsgID       string   `json:"msg_id"`
	SessionID   string   `json:"session_id"`
	SessionDir  string   `json:"session_dir"` // 会话工作区（产物落盘根，Go 计算）
	GrantedLv   int      `json:"granted_level"`
	DeadlineRFC string   `json:"deadline"`
}

// ShellResponse 是壳通道的应答信封（映射回 proto.ToolResponse）。
type ShellResponse struct {
	State   string            `json:"state"`   // completed / error（缺省 completed）
	Content string            `json:"content"` // 成功输出
	Error   string            `json:"error"`
	Attrs   map[string]string `json:"attrs"`
}

// RunViaShell 构造经壳通道执行的 ProviderRunFunc：请求超时受 ctx（请求 deadline）约束，
// socket deadline 取 ctx deadline + 5s 宽限（壳侧长操作的应答余量）。
func RunViaShell(ch ShellChannel) ProviderRunFunc {
	return func(ctx context.Context, sid string, req *proto.ToolRequest, argv []string) *proto.ToolResponse {
		msgID := ""
		deadline := ""
		granted := 0
		if req != nil {
			msgID = req.MsgID
			deadline = req.Deadline
			granted = int(req.GrantedLevel)
		}
		conn, err := net.DialTimeout("tcp", ch.Addr, 3*time.Second)
		if err != nil {
			return &proto.ToolResponse{MsgID: msgID, State: proto.StateError,
				Error: fmt.Sprintf("shell channel dial failed (shell not ready?): %v", err)}
		}
		defer conn.Close()
		if dl, ok := ctx.Deadline(); ok {
			_ = conn.SetDeadline(dl.Add(5 * time.Second))
		} else {
			_ = conn.SetDeadline(time.Now().Add(130 * time.Second))
		}
		body, _ := json.Marshal(ShellRequest{
			Token:       ch.Token,
			Argv:        argv,
			MsgID:       msgID,
			SessionID:   sid,
			SessionDir:  sessionWorkDir(sid),
			GrantedLv:   granted,
			DeadlineRFC: deadline,
		})
		if _, err := conn.Write(append(body, '\n')); err != nil {
			return &proto.ToolResponse{MsgID: msgID, State: proto.StateError,
				Error: fmt.Sprintf("shell channel write failed: %v", err)}
		}
		line, err := bufio.NewReader(conn).ReadString('\n')
		if err != nil {
			return &proto.ToolResponse{MsgID: msgID, State: proto.StateError,
				Error: fmt.Sprintf("shell channel read failed: %v", err)}
		}
		var resp ShellResponse
		if err := json.Unmarshal([]byte(line), &resp); err != nil {
			return &proto.ToolResponse{MsgID: msgID, State: proto.StateError,
				Error: fmt.Sprintf("shell channel invalid response: %v", err)}
		}
		state := proto.StateCompleted
		if resp.State == string(proto.StateError) {
			state = proto.StateError
		}
		return &proto.ToolResponse{
			MsgID:   msgID,
			State:   state,
			Content: resp.Content,
			Error:   resp.Error,
			Attrs:   resp.Attrs,
		}
	}
}
