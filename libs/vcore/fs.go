package vcore

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"github.com/veypi/aic-pod/libs/proto"
)

// fsParams 是 fs 指令集的原生 JSON 参数（§4：8 action，三端 schema 完全一致）。
// fs 是文件服务工具：read/write/edit/ls/rg/cp/mv/rm，全部经此分发。
// 无 workdir 参数：路径一律绝对（ls/rg 省略 path 时缺省 = env.Workdir）。
type fsParams struct {
	Action string `json:"action"`

	// 目标路径：read/write/edit/ls/rg/rm
	Path string `json:"path,omitempty"`

	// read
	Offset *int `json:"offset,omitempty"`
	Limit  *int `json:"limit,omitempty"`

	// write
	Content *string `json:"content,omitempty"`

	// edit
	Edits []editOp `json:"edits,omitempty"`

	// ls（depth>1 即递归树，吸收原 tree 指令）
	Depth *int `json:"depth,omitempty"` // 默认 1，上限 5
	All   bool `json:"all,omitempty"`   // 收录点开头隐藏项（默认跳过）；rg 同义

	// rg：pattern 缺省 = 文件列举模式；smart case（pattern 全小写 → 不敏感）
	Pattern string   `json:"pattern,omitempty"`
	Glob    []string `json:"glob,omitempty"` // 文件名 glob（basename，OR 语义；不支持 ! 与 **）

	// cp / mv
	Src string `json:"src,omitempty"`
	Dst string `json:"dst,omitempty"`

	// rm（非空目录）；cp 目录自动递归，无需确认
	Recursive bool `json:"recursive,omitempty"`
}

type editOp struct {
	OldText string `json:"oldText"`
	NewText string `json:"newText"`
}

// FSActions 是 fs 的全部 action（§4）。
var FSActions = []string{"read", "write", "edit", "ls", "rg", "cp", "mv", "rm"}

// RunFS 执行 fs action（原生 JSON 参数，无 argv）。
// 未知字段报错 `fs {action}: unknown field "{name}"`（§2.1）。
func RunFS(ctx context.Context, env *Env, raw json.RawMessage) (*Result, error) {
	var p fsParams
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return nil, fsErr("", "invalid params: %s", err)
	}
	if p.Action == "" {
		return nil, fsErr("", "action is required (supported: read, write, edit, ls, rg, cp, mv, rm)")
	}
	if env.VFS == nil {
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
	case "cp":
		return fsCp(ctx, env, &p)
	case "mv":
		return fsMv(ctx, env, &p)
	case "rm":
		return fsRm(ctx, env, &p)
	}
	return nil, &proto.ExecError{Tool: proto.ToolFS,
		Reason: fmt.Sprintf("unknown action %q (supported: read, write, edit, ls, rg, cp, mv, rm)", p.Action)}
}
