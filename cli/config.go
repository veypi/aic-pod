// Copyright (C) 2025 veypi <i@veypi.com>
// Distributed under terms of the MIT license.

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/veypi/aic-pod/cfg"
	"github.com/veypi/aic-pod/settings"
)

// 子命令实现：`aic config get|set` / `aic bind` / `aic unbind`（2026-09-22 去本地
// 管理 API 后，本机设置面就是 config.yaml 本身）。Electron 设置窗口由主进程 spawn
// 这些子命令读写文件；没有端口、没有 code、没有常驻服务。
// 设置 JSON 与凭证走 stdin（不出现在进程参数里，避免 ps 泄漏凭证）。

// writeJSON 输出缩进 JSON 到 stdout（desktop 主进程解析）。
func writeJSON(v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(b))
	return nil
}

// readStdin 读取 stdin 全文。
func readStdin() ([]byte, error) {
	b, err := io.ReadAll(os.Stdin)
	if err != nil {
		return nil, fmt.Errorf("read stdin: %w", err)
	}
	return b, nil
}

// runConfigGet 打印有效配置（文件 + env + 默认值；供设置窗口回显）。
// cfg.Global 已由 flags.Parse 装载（defaults < 文件 < env < flag）；这里只输出视图，
// 不再重复 Load——避免丢掉 env/flag 覆盖层。
func runConfigGet() error {
	return writeJSON(settings.Snapshot())
}

// runConfigSet 应用 stdin 的设置 JSON 并落盘（生效 = 调用方重启后端）。
func runConfigSet() error {
	b, err := readStdin()
	if err != nil {
		return err
	}
	var upd settings.Update
	if err := json.Unmarshal(b, &upd); err != nil {
		return fmt.Errorf("parse settings json: %w", err)
	}
	if err := upd.Apply(); err != nil {
		return err
	}
	return writeJSON(map[string]any{"ok": true})
}

// runBind 保存平台凭证（stdin 纯文本）；绑定生效由调用方重启后端完成。
func runBind() error {
	b, err := readStdin()
	if err != nil {
		return err
	}
	cred := strings.TrimSpace(string(b))
	if cred == "" {
		return errors.New("credential is empty (pass it on stdin)")
	}
	o, err := cfg.LoadFile()
	if err != nil {
		return err
	}
	o.Key = cred
	if err := cfg.Save(o); err != nil {
		return err
	}
	return writeJSON(map[string]any{"ok": true, "host": o.Host, "host_id": settings.BoundHostID(cred)})
}

// runUnbind 清除平台凭证（保留 host/work_dir 等运行参数）。
func runUnbind() error {
	o, err := cfg.LoadFile()
	if err != nil {
		return err
	}
	o.Key = ""
	if err := cfg.Save(o); err != nil {
		return err
	}
	return writeJSON(map[string]any{"ok": true})
}
