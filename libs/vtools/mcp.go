package vtools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/veypi/aic-pod/libs/mcpx"
)

// MCPResolve 取得指定服务的共享 MCP 连接（懒启动与空闲回收由连接管理层
// mcpx.Manager 负责；本适配器只做协议转换）。
type MCPResolve func(ctx context.Context) (*mcp.ClientSession, error)

// MCPProvider 把 MCP 服务适配成 vtools.Provider（含 --image-preview 支持）。
type MCPProvider struct {
	resolve MCPResolve
}

// MCP 构造 MCP 适配器。resolve 在每次调用时现取连接，因此连接管理层重建
// （配置更新）后本适配器无需重注册。
func MCP(resolve MCPResolve) *MCPProvider {
	return &MCPProvider{resolve: resolve}
}

func (p *MCPProvider) Tools(ctx context.Context) ([]Tool, error) {
	session, err := p.resolve(ctx)
	if err != nil {
		return nil, err
	}
	var out []Tool
	for t, err := range session.Tools(ctx, nil) {
		if err != nil {
			return nil, err
		}
		var schema json.RawMessage
		if t.InputSchema != nil {
			raw, merr := json.Marshal(t.InputSchema)
			if merr != nil {
				return nil, fmt.Errorf("tool %s: inputSchema is not serializable: %w", t.Name, merr)
			}
			schema = raw
		}
		out = append(out, Tool{Name: t.Name, Description: t.Description, InputSchema: schema})
	}
	return out, nil
}

func (p *MCPProvider) Call(ctx context.Context, tool string, args map[string]any) (any, error) {
	session, err := p.resolve(ctx)
	if err != nil {
		return nil, err
	}
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		return nil, err
	}
	if result == nil {
		return nil, fmt.Errorf("tool %s returned no result", tool)
	}
	if result.NeedsInput() {
		return nil, fmt.Errorf("tool requires unsupported interactive input")
	}
	if result.IsError {
		return result, &CallError{Result: result}
	}
	return result, nil
}

// PreviewImages 实现 ImagePreviewer：--image-preview 仅作用于成功结果，
// 语义与 mcp call --image-preview 一致（mcpx.PreviewToolImages）。
func (p *MCPProvider) PreviewImages(result any, size string) (any, error) {
	r, ok := result.(*mcp.CallToolResult)
	if !ok {
		return nil, fmt.Errorf("--image-preview requires an MCP tool result")
	}
	return mcpx.PreviewToolImages(r, size)
}
