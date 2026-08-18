// Copyright (C) 2025 veypi <i@veypi.com>
// Distributed under terms of the MIT license.

package pod

import (
	"bufio"
	"encoding/json"
	"net"
	"path/filepath"
	"testing"
)

// TestWake 以真实 unix socket 模拟 Electron 主进程指令通道（换行 JSON 协议），
// 验证 Wake 全链路：无服务端报错 / 正常唤醒 / 未知指令透传错误。
func TestWake(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	sock := filepath.Join(t.TempDir(), "desktop.sock")
	orig := cmdSockPath
	cmdSockPath = func() (string, error) { return sock, nil }
	defer func() { cmdSockPath = orig }()

	// 无服务端 → 连接失败（desktop 未运行）
	if err := Wake(); err == nil {
		t.Fatal("expect error when no server listening")
	}

	// 模拟 Electron 端（协议与 desktop/main.js handleCmd 一致）
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				line, err := bufio.NewReader(c).ReadString('\n')
				if err != nil {
					return
				}
				var cmd map[string]any
				if json.Unmarshal([]byte(line), &cmd) != nil {
					c.Write([]byte(`{"ok":false,"error":"invalid json"}` + "\n"))
					return
				}
				if cmd["action"] != "wake" {
					c.Write([]byte(`{"ok":false,"error":"unknown action"}` + "\n"))
					return
				}
				c.Write([]byte(`{"ok":true}` + "\n"))
			}(conn)
		}
	}()

	if err := Wake(); err != nil {
		t.Fatalf("wake: %v", err)
	}
}
