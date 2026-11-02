// Package vtools 是 MCP Provider → vsh 命令的调用内核（工具目录 / 参数表 /
// 调用渲染），不含登记表：命令的注册、撞名、增删与生命周期统一归
// libs/host 的 command hub（每个服务 alias 一个 vsh 根命令）。
//
// 形态（见 docs/reference/host-vtools.md）：
//   - `<alias> <tool> [--flag value]... [--input JSON|-] [--json]`；
//     无参打印工具目录；`<alias> <tool> --help` 按 inputSchema 渲染参数表。
//   - 注册即时、连接懒惰：首次执行才连接（command hub 的 mcpx.Manager）。
//   - 命令准入由 vsh 统一处理。RTC 流通道统一由 command hub 以
//     `<alias>:<stream>` 承载（见 libs/host/command_registry.go）。
package vtools

import (
	"context"
	"encoding/json"
)

// Tool 是能力面条目（协议无关；MCP 的 inputSchema 原样透传）。
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"inputSchema,omitempty"`
}

// Provider 是协议适配器的最小面。
type Provider interface {
	Tools(ctx context.Context) ([]Tool, error)
	Call(ctx context.Context, tool string, args map[string]any) (any, error)
}

// CallError 是工具级失败（如 MCP isError）：结果照常渲染，退出码 1。
type CallError struct {
	Result any
}

func (e *CallError) Error() string { return "tool reported an error" }

// ImagePreviewer 是可选能力：支持 --image-preview WIDTHxHEIGHT 的 Provider
// （MCP 适配器实现）。
type ImagePreviewer interface {
	PreviewImages(result any, size string) (any, error)
}
