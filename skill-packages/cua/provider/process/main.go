// Copyright (C) 2025 veypi <i@veypi.com>
// Distributed under terms of the MIT license.

// cua 是 skill-packages/cua 的 process 类 provider（v6 P6，包根命令的默认
// provider）：无状态 skillproc 转发器——argv/cwd 全量经 invoke 帧发给
// svc provider（SKILLPROC_SOCKET，skillrun 在包命令调用前确保 service 已懒
// 启动并注入该 env），stdout/stderr/exit code 原样回传。子命令表、解析与
// cua-driver 会话全部在 svc（一切触碰驱动的都走 svc）。
//
// 取消语义：vbox 受管取消杀本进程组 → 连接断开 → svc 取消该连接的 invoke。
package main

import (
	"fmt"
	"os"

	"github.com/veypi/aic-pod/protocol/skillproc"
)

func main() {
	sock := os.Getenv("SKILLPROC_SOCKET")
	if sock == "" {
		fmt.Fprintln(os.Stderr, "cua: SKILLPROC_SOCKET is required（svc provider 未就绪）")
		os.Exit(2)
	}
	conn, err := skillproc.Dial("unix", sock)
	if err != nil {
		fmt.Fprintln(os.Stderr, "cua: service dial:", err)
		os.Exit(1)
	}
	defer conn.Close()
	cwd, _ := os.Getwd()
	const id = "cli-1"
	if err := conn.Send(skillproc.Header{ID: id, Type: skillproc.TypeInvoke, Argv: os.Args[1:], Cwd: cwd}, nil); err != nil {
		fmt.Fprintln(os.Stderr, "cua: invoke:", err)
		os.Exit(1)
	}
	for {
		h, payload, err := conn.Recv()
		if err != nil {
			fmt.Fprintln(os.Stderr, "cua: service conn:", err)
			os.Exit(1)
		}
		if h.ID != id {
			continue
		}
		switch h.Type {
		case skillproc.TypeFrame:
			w := os.Stdout
			if h.Stream == skillproc.StreamStderr {
				w = os.Stderr
			}
			_, _ = w.Write(payload)
		case skillproc.TypeExit:
			if h.Error != "" {
				fmt.Fprintln(os.Stderr, "cua:", h.Error)
				if h.Code == 0 {
					os.Exit(1)
				}
			}
			os.Exit(h.Code)
		case skillproc.TypeError:
			fmt.Fprintln(os.Stderr, "cua:", h.Error)
			os.Exit(1)
		}
	}
}
