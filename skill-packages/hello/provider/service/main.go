// Copyright (C) 2025 veypi <i@veypi.com>
// Distributed under terms of the MIT license.

// hello-service 是 skill-packages/hello 的 service 类 provider（v6 P0 demo，
// aic/docs/skill.md §9.2）：真实外部进程，演示 skillproc 最小协议的 provider
// 侧写法——监听 SKILLPROC_SOCKET（pod 经 env 注入的 unix socket 路径），
// 每连接读帧分发：
//
//	invoke      argv/cwd 回显、pipe（stdin 负载回显）、sleep N（可取消）、exit N
//	cancel      中断本连接当前 invoke（exit 124）
//	stream.open echo 通道：stream.frame 原样回显，stream.close 收尾
//
// 无 handshake/health/版本协商——崩溃由 pod 侧下次调用重拉。
package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/veypi/aic-pod/protocol/skillproc"
)

func main() {
	sock := os.Getenv("SKILLPROC_SOCKET")
	if sock == "" {
		fmt.Fprintln(os.Stderr, "SKILLPROC_SOCKET is required")
		os.Exit(2)
	}
	_ = os.Remove(sock)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		fmt.Fprintln(os.Stderr, "listen:", err)
		os.Exit(1)
	}
	defer ln.Close()
	fmt.Fprintln(os.Stderr, "hello-service listening", sock)
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go serveConn(skillproc.NewConn(c))
	}
}

// serveConn 单连接服务循环：invoke 与 stream 通道都在这条连接上多路复用。
func serveConn(conn *skillproc.Conn) {
	defer conn.Close()
	connCtx, connCancel := context.WithCancel(context.Background())
	defer connCancel()
	var mu sync.Mutex
	var invokeCancel context.CancelFunc
	streams := map[string]bool{}
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
			go runInvoke(ctx, conn, h, payload, &mu, &invokeCancel)
		case skillproc.TypeCancel:
			mu.Lock()
			if invokeCancel != nil {
				invokeCancel()
				invokeCancel = nil
			}
			mu.Unlock()
		case skillproc.TypeStreamOpen:
			if h.Name == "echo" {
				streams[h.ID] = true
			} else {
				_ = conn.Send(skillproc.Header{ID: h.ID, Type: skillproc.TypeError, Error: "unknown stream " + h.Name}, nil)
			}
		case skillproc.TypeStreamFrame:
			if streams[h.ID] {
				_ = conn.Send(skillproc.Header{ID: h.ID, Type: skillproc.TypeStreamFrame}, payload)
			}
		case skillproc.TypeStreamClose:
			delete(streams, h.ID)
			_ = conn.Send(skillproc.Header{ID: h.ID, Type: skillproc.TypeStreamClose}, nil)
		}
	}
}

// runInvoke 处理一次调用：子命令与 hello-process 同族（service 形态）。
func runInvoke(ctx context.Context, conn *skillproc.Conn, h skillproc.Header, stdin []byte, mu *sync.Mutex, current *context.CancelFunc) {
	send := func(typ, stream string, payload []byte) {
		_ = conn.Send(skillproc.Header{ID: h.ID, Type: typ, Stream: stream}, payload)
	}
	exit := func(code int, errStr string) {
		_ = conn.Send(skillproc.Header{ID: h.ID, Type: skillproc.TypeExit, Code: code, Error: errStr}, nil)
		mu.Lock()
		*current = nil
		mu.Unlock()
	}
	args := h.Argv
	if len(args) == 0 {
		send(skillproc.TypeFrame, skillproc.StreamStdout, []byte("hello from aic skill service\n"))
		exit(0, "")
		return
	}
	switch args[0] {
	case "argv":
		for _, a := range args[1:] {
			send(skillproc.TypeFrame, skillproc.StreamStdout, []byte(a+"\n"))
		}
		exit(0, "")
	case "pipe":
		send(skillproc.TypeFrame, skillproc.StreamStdout, stdin)
		exit(0, "")
	case "sleep":
		sec := 30
		if len(args) > 1 {
			if v, err := strconv.Atoi(args[1]); err == nil {
				sec = v
			}
		}
		select {
		case <-time.After(time.Duration(sec) * time.Second):
			send(skillproc.TypeFrame, skillproc.StreamStdout, []byte(fmt.Sprintf("slept %d\n", sec)))
			exit(0, "")
		case <-ctx.Done():
			exit(124, "cancelled")
		}
	case "exit":
		code := 0
		if len(args) > 1 {
			code, _ = strconv.Atoi(args[1])
		}
		exit(code, "")
	default:
		exit(2, "unknown subcommand "+args[0])
	}
}
