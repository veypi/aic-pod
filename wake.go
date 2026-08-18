// Copyright (C) 2025 veypi <i@veypi.com>
// Distributed under terms of the MIT license.

package pod

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

// cmdSockPath 桌面指令通道（Electron 主进程 unix socket）地址；测试可替换
var cmdSockPath = func() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "aic", "desktop.sock"), nil
}

// Wake 唤醒桌宠录音（aic wake 子指令）：经 desktop（Electron 主进程）的本地指令
// 通道（unix socket UserConfigDir/aic/desktop.sock，换行分隔 JSON 请求/应答）转发
// pet:cmd 事件给 pet 组件，效果等同 pet 页左键单击。
// 仅 desktop 形态支持——socket 由 Electron 主进程创建；纯 cli（无桌面）拨号必失败。
func Wake() error {
	if runtime.GOOS == "windows" {
		return fmt.Errorf("wake 暂不支持 windows（桌面指令通道为 unix socket）")
	}
	sock, err := cmdSockPath()
	if err != nil {
		return err
	}
	conn, err := net.DialTimeout("unix", sock, 2*time.Second)
	if err != nil {
		return fmt.Errorf("连接桌面端失败（desktop 未运行？）: %w", err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := conn.Write([]byte(`{"action":"wake"}` + "\n")); err != nil {
		return fmt.Errorf("发送唤醒指令失败: %w", err)
	}
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		return fmt.Errorf("读取应答失败: %w", err)
	}
	var resp struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal([]byte(line), &resp); err != nil {
		return fmt.Errorf("解析应答失败: %w", err)
	}
	if !resp.OK {
		return fmt.Errorf("唤醒失败: %s", resp.Error)
	}
	return nil
}
