// Copyright (C) 2025 veypi <i@veypi.com>
// Distributed under terms of the MIT license.

// cua-service 是 skill-packages/cua 的 service 类 provider（v6 P6）：持有
// cua-driver MCP 连接与设备级唯一原生会话的唯一进程。skillrun 懒启动
// （首调用）+ bg 登记；SKILLPROC_SOCKET 经 env 注入。
//
// 协议面（skillproc，无 handshake——崩溃由 pod 侧下次调用重拉）：
//
//	invoke      argv/cwd 全量转发自 CLI（process provider）——子命令表/解析/
//	            JSON 输出契约见 cua.Service.Run（cua.Help）；连接断开
//	            即取消该连接的 invoke（CLI 被杀 = 调用取消）。
//
// 驱动探测（包内解析，pod/desktop 只递目录提示）：
//
//	AIC_CUA_DRIVER_PATH  cua-driver 可执行文件（显式覆盖，最高优先）
//	AIC_CUA_BUNDLE_DIR   Electron 内置发行物目录提示（resources/cua/{plat}；
//	                     darwin 派生 CuaDriver.app——签名/公证原样，TCC 授权
//	                     归 com.trycua.driver）→ PATH → 系统候选（缺省回落）
package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"sync"

	"github.com/veypi/aic-pod/protocol/skillproc"
	"github.com/veypi/aic-pod/skill-packages/cua/provider/cua"
)

func main() {
	logf := func(format string, args ...any) { fmt.Fprintf(os.Stderr, "cua-service: "+format+"\n", args...) }
	sock := os.Getenv("SKILLPROC_SOCKET")
	if sock == "" {
		fmt.Fprintln(os.Stderr, "cua-service: SKILLPROC_SOCKET is required")
		os.Exit(2)
	}
	svc := cua.New(cua.Config{Logf: logf})
	_ = os.Remove(sock)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		fmt.Fprintln(os.Stderr, "cua-service: listen:", err)
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

// serveConn 单连接服务循环：一条连接上一个 invoke（cancel/断连可中断）。
// cua 无 stream 端点（窗口观察走 snapshot/image 子命令拉取）。
func serveConn(svc *cua.Service, conn *skillproc.Conn) {
	defer conn.Close()
	connCtx, connCancel := context.WithCancel(context.Background())
	defer connCancel()
	var mu sync.Mutex
	var invokeCancel context.CancelFunc
	for {
		h, _, err := conn.Recv()
		if err != nil {
			return
		}
		switch h.Type {
		case skillproc.TypeInvoke:
			ctx, cancel := context.WithCancel(connCtx)
			mu.Lock()
			invokeCancel = cancel
			mu.Unlock()
			go func() {
				stdout := &frameWriter{conn: conn, id: h.ID, stream: skillproc.StreamStdout}
				stderr := &frameWriter{conn: conn, id: h.ID, stream: skillproc.StreamStderr}
				code := svc.Run(ctx, h.Argv, h.Cwd, stdout, stderr)
				_ = conn.Send(skillproc.Header{ID: h.ID, Type: skillproc.TypeExit, Code: code}, nil)
				cancel()
				mu.Lock()
				invokeCancel = nil
				mu.Unlock()
			}()
		case skillproc.TypeCancel:
			mu.Lock()
			if invokeCancel != nil {
				invokeCancel()
				invokeCancel = nil
			}
			mu.Unlock()
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
