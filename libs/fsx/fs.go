package fsx

// fs.go 是 fsx 的入口分发（自 vcore fs.go 移植，v4.1 瘦身）：五 action
// write/edit/read/ls/rg；cp/mv/rm 下线——报可读错误引导 exec（壳层动作
// 由引擎内建承接，D11）。

import (
	"context"
	"encoding/json"
)

// fsParams 是 fs 指令集的原生 JSON 参数（三端 schema 一致）。
// 无 workdir 参数：路径一律绝对（ls/rg 省略 path 时缺省 = env.Workdir）。
type fsParams struct {
	Action string `json:"action"`

	// 目标路径：read/write/edit/ls/rg
	Path string `json:"path,omitempty"`

	// read（offset/limit，limit 上限 1000）；rg 复用 limit = 全局输出行数上限
	// （命中+上下文行同池计数，默认 50 上限 200）
	Offset *int `json:"offset,omitempty"`
	Limit  *int `json:"limit,omitempty"`

	// write
	Content *string `json:"content,omitempty"`

	// edit
	Edits []editOp `json:"edits,omitempty"`

	// ls（depth>1 即递归树）
	Depth *int `json:"depth,omitempty"` // 默认 1，上限 5
	All   bool `json:"all,omitempty"`   // 收录点开头隐藏项（默认跳过）；rg 同义

	// rg：pattern 缺省 = 文件列举模式；smart case（pattern 全小写 → 不敏感）
	Pattern string `json:"pattern,omitempty"`
	// 文件名 glob（basename，include OR 语义；! 前缀 = 排除 glob；不支持 **）
	Glob []string `json:"glob,omitempty"`
	// rg：context = 命中行上下各 N 行上下文（grep -C 语义，0-10 默认 0）。
	Context *int `json:"context,omitempty"`
}

type editOp struct {
	OldText string `json:"oldText"`
	NewText string `json:"newText"`
}

// FSActions 是 fs 的全部 action（v4.1：五 action；cp/mv/rm 移入 exec 内建）。
var FSActions = []string{"read", "write", "edit", "ls", "rg"}

// RunFS 执行 fs action（原生 JSON 参数）。
// 未知字段宽忽略（AI 按 schema 全量传参是常态，多传参数当看不见）。
func RunFS(ctx context.Context, env *Env, raw json.RawMessage) (*Result, error) {
	var p fsParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, fsErr("", "invalid params: %s", err)
	}
	if p.Action == "" {
		return nil, fsErr("", "action is required (supported: read, write, edit, ls, rg)")
	}
	if env.FS == nil {
		return nil, fsErr(p.Action, "file service is not enabled")
	}
	switch p.Action {
	case "read":
		return fsRead(ctx, env, &p)
	case "write":
		return fsWrite(ctx, env, &p)
	case "edit":
		return fsEdit(ctx, env, &p)
	case "ls":
		return fsLs(ctx, env, &p)
	case "rg":
		return fsRg(ctx, env, &p)
	case "cp", "mv", "rm":
		// 壳层动作下线（D11/v4.1）：引导 exec 内建（引擎 90 内建承接）。
		return nil, fsErr(p.Action, "%s", "fs 不再提供 "+p.Action+"——壳层动作（cp/mv/rm）请用 exec（如 `exec cp a b`）；fs 只保留结构化读写编辑（read/write/edit/ls/rg）")
	}
	return nil, fsErr("", "%s", "unknown action "+p.Action+" (supported: read, write, edit, ls, rg)")
}
