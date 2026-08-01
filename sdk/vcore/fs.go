package vcore

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"github.com/veypi/aic-pod/sdk/proto"
)

// fsParams 是 fs 指令集的原生 JSON 参数（§4：三 action schema 三端完全一致）。
type fsParams struct {
	Action  string   `json:"action"`
	Path    string   `json:"path"`
	Workdir string   `json:"workdir,omitempty"`
	Offset  *int     `json:"offset,omitempty"`
	Limit   *int     `json:"limit,omitempty"`
	Content *string  `json:"content,omitempty"`
	Edits   []editOp `json:"edits,omitempty"`
}

type editOp struct {
	OldText string `json:"oldText"`
	NewText string `json:"newText"`
}

// FSActions 是 fs 的全部 action（§4）。
var FSActions = []string{"read", "write", "edit"}

// RunFS 执行 fs action（原生 JSON 参数，无 argv）。
// 未知字段报错 `fs {action}: unknown field "{name}"`（§2.1）。
func RunFS(ctx context.Context, env *Env, raw json.RawMessage) (*Result, error) {
	var p fsParams
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		var ute *json.UnmarshalTypeError
		_ = ute
		return nil, fsErr("", "invalid params: %s", err)
	}
	if p.Action == "" {
		return nil, fsErr("", "action is required (supported: read, write, edit)")
	}
	env2 := *env
	if p.Workdir != "" {
		env2.Workdir = p.Workdir
	}
	switch p.Action {
	case "read":
		return fsRead(ctx, &env2, &p)
	case "write":
		return fsWrite(ctx, &env2, &p)
	case "edit":
		return fsEdit(ctx, &env2, &p)
	}
	return nil, &proto.ExecError{Tool: proto.ToolFS,
		Reason: fmt.Sprintf("unknown action %q (supported: read, write, edit)", p.Action)}
}
