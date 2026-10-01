// Copyright (C) 2025 veypi <i@veypi.com>
// Distributed under terms of the MIT license.

// browser-service 是 skill-packages/browser 的 service 类 provider（v6 P5）：
// 持有 Chrome 与全部页面/下载/上传状态的唯一进程。skillrun 懒启动（首调用）
// + bg 登记；SKILLPROC_SOCKET 经 env 注入。
//
// 协议面（skillproc，无 handshake——崩溃由 pod 侧下次调用重拉）：
//
//	invoke      argv/cwd 全量转发自 CLI（process provider）——子命令表/解析/
//	            JSON 输出契约见 browser.Service.Run（browser.Help）；连接断开
//	            即取消该连接的 invoke（CLI 被杀 = 调用取消）。
//	stream.open page.frames / page.input（端点名 = 流名全名）：负载 =
//	            PageArgs JSON；stream id = 连接身份（输入租约凭它区分持有者）。
//	            一帧 stream.frame = 一条 tool 消息（帧边界 = 消息边界）。
//
// 配置（包内默认 + env 覆盖，v6 P5 起 pod 不再持有 browser 配置项）：
//
//	AIC_BROWSER_PATH        Chrome 可执行文件（显式覆盖，最高优先；缺省走
//	                        chrome.Resolve 候选链：AIC_BROWSER_BUNDLE_DIR 打包内置
//	                        Chrome for Testing → 系统安装探测）
//	AIC_BROWSER_STATE_DIR   状态目录（默认 $HOME/.aic/browser：profile/downloads/
//	                        uploads，进程锁防并发实例）
//	AIC_BROWSER_WIDTH/HEIGHT 新建页面默认视口（默认 1280/720）
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"

	"github.com/veypi/aic-pod/protocol/skillproc"
	"github.com/veypi/aic-pod/skill-packages/browser/provider/browser"
)

func main() {
	logf := func(format string, args ...any) { fmt.Fprintf(os.Stderr, "browser-service: "+format+"\n", args...) }
	sock := os.Getenv("SKILLPROC_SOCKET")
	if sock == "" {
		fmt.Fprintln(os.Stderr, "browser-service: SKILLPROC_SOCKET is required")
		os.Exit(2)
	}
	svc := browser.New(configFromEnv(logf))
	_ = os.Remove(sock)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		fmt.Fprintln(os.Stderr, "browser-service: listen:", err)
		os.Exit(1)
	}
	defer ln.Close()
	logf("listening %s", sock)
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go serveConn(svc, skillproc.NewConn(c))
	}
}

// configFromEnv 包内默认 + env 覆盖（见文件头）。
func configFromEnv(logf func(string, ...any)) browser.Config {
	stateDir := os.Getenv("AIC_BROWSER_STATE_DIR")
	if stateDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			fmt.Fprintln(os.Stderr, "browser-service: home dir:", err)
			os.Exit(1)
		}
		stateDir = filepath.Join(home, ".aic", "browser")
	}
	envInt := func(key string, fallback int) int {
		if v := os.Getenv(key); v != "" {
			if n, err := strconv.Atoi(v); err == nil {
				return n
			}
		}
		return fallback
	}
	return browser.Config{
		StateDir: stateDir,
		Width:    envInt("AIC_BROWSER_WIDTH", 1280),
		Height:   envInt("AIC_BROWSER_HEIGHT", 720),
		Logf:     logf,
	}
}

// serveConn 单连接服务循环：一条连接上一个 invoke（cancel/断连可中断）与
// 若干 stream 通道多路复用。
func serveConn(svc *browser.Service, conn *skillproc.Conn) {
	defer conn.Close()
	connCtx, connCancel := context.WithCancel(context.Background())
	defer connCancel()
	var mu sync.Mutex
	var invokeCancel context.CancelFunc
	streams := map[string]browser.Stream{}
	closeStreams := func() {
		mu.Lock()
		defer mu.Unlock()
		for id, st := range streams {
			_ = st.Close()
			delete(streams, id)
		}
	}
	defer closeStreams()
	for {
		h, payload, err := conn.Recv()
		if err != nil {
			return
		}
		switch h.Type {
		case skillproc.TypeInvoke:
			ctx, cancel := context.WithCancel(connCtx)
			mu.Lock()
			invokeCancel = cancel
			mu.Unlock()
			go runInvoke(svc, conn, h, ctx, cancel, &mu, &invokeCancel)
		case skillproc.TypeCancel:
			mu.Lock()
			if invokeCancel != nil {
				invokeCancel()
				invokeCancel = nil
			}
			mu.Unlock()
		case skillproc.TypeStreamOpen:
			st, err := openStream(svc, connCtx, h, payload)
			if err != nil {
				_ = conn.Send(skillproc.Header{ID: h.ID, Type: skillproc.TypeError, Error: err.Error()}, nil)
				continue
			}
			mu.Lock()
			streams[h.ID] = st
			mu.Unlock()
			go pumpStream(conn, h.ID, st)
		case skillproc.TypeStreamFrame:
			mu.Lock()
			st := streams[h.ID]
			mu.Unlock()
			if st == nil {
				continue
			}
			if err := st.Send(connCtx, payload); err != nil {
				_ = conn.Send(skillproc.Header{ID: h.ID, Type: skillproc.TypeError, Error: err.Error()}, nil)
			}
		case skillproc.TypeStreamClose:
			mu.Lock()
			st := streams[h.ID]
			delete(streams, h.ID)
			mu.Unlock()
			if st != nil {
				_ = st.Close()
			}
		}
	}
}

// runInvoke 一次 CLI 调用：子命令执行与输出契约全部在 Service.Run。
func runInvoke(svc *browser.Service, conn *skillproc.Conn, h skillproc.Header, ctx context.Context, cancel context.CancelFunc, mu *sync.Mutex, current *context.CancelFunc) {
	stdout := &frameWriter{conn: conn, id: h.ID, stream: skillproc.StreamStdout}
	stderr := &frameWriter{conn: conn, id: h.ID, stream: skillproc.StreamStderr}
	code := svc.Run(ctx, h.Argv, h.Cwd, stdout, stderr)
	_ = conn.Send(skillproc.Header{ID: h.ID, Type: skillproc.TypeExit, Code: code}, nil)
	cancel()
	mu.Lock()
	*current = nil
	mu.Unlock()
}

// openStream 打开 manifest 声明的 stream 端点（page.frames/page.input；负载 =
// PageArgs JSON）。stream id = 连接身份（输入租约持有者判定）。
func openStream(svc *browser.Service, ctx context.Context, h skillproc.Header, payload []byte) (browser.Stream, error) {
	var a browser.PageArgs
	if len(payload) > 0 {
		if err := json.Unmarshal(payload, &a); err != nil {
			return nil, fmt.Errorf("stream args: %w", err)
		}
	}
	if err := a.Validate(); err != nil {
		return nil, err
	}
	switch h.Name {
	case "page.frames":
		return svc.Frames(ctx, a)
	case "page.input":
		return svc.Input(ctx, h.ID, a)
	}
	return nil, fmt.Errorf("unknown stream %q", h.Name)
}

// pumpStream 流读泵：stream 消息 → stream.frame（一帧一条，边界保持）；流终结
// 以 stream.close 收尾（io.EOF 之外读不出数据——frame/input 流本身无入向数据）。
func pumpStream(conn *skillproc.Conn, id string, st browser.Stream) {
	ctx := context.Background()
	for {
		b, err := st.Recv(ctx)
		if err != nil {
			errStr := ""
			if err != io.EOF {
				errStr = err.Error()
			}
			_ = conn.Send(skillproc.Header{ID: id, Type: skillproc.TypeStreamClose, Error: errStr}, nil)
			return
		}
		if err := conn.Send(skillproc.Header{ID: id, Type: skillproc.TypeStreamFrame}, b); err != nil {
			_ = st.Close()
			return
		}
	}
}

// frameWriter 把 Run 的 stdout/stderr 写转成 skillproc frame。
type frameWriter struct {
	conn   *skillproc.Conn
	id     string
	stream string
}

func (w *frameWriter) Write(p []byte) (int, error) {
	if err := w.conn.Send(skillproc.Header{ID: w.id, Type: skillproc.TypeFrame, Stream: w.stream}, p); err != nil {
		return 0, err
	}
	return len(p), nil
}
