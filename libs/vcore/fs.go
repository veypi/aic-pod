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
type fsParams struct {
	Action  string `json:"action"`
	Workdir string `json:"workdir,omitempty"`

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
	Depth *int   `json:"depth,omitempty"` // 默认 1，上限 5
	All   bool   `json:"all,omitempty"`   // 收录点开头隐藏项（默认跳过）
	Sort  string `json:"sort,omitempty"`  // "name"（默认，UTF-8 字节序）| "time"（mtime 降序，同值按名称）

	// rg
	Pattern    string   `json:"pattern,omitempty"`      // 内容搜索模式（files=true 时禁用）
	Glob       []string `json:"glob,omitempty"`         // 文件名 glob（basename，OR 语义；不支持 ! 与 **）
	Files      bool     `json:"files,omitempty"`        // true = 纯文件列举（原 --files）
	Hidden     bool     `json:"hidden,omitempty"`       // 收录隐藏文件/目录（原 --hidden）
	Insensitive bool   `json:"insensitive,omitempty"`  // 大小写不敏感（原 -i）
	Word       bool     `json:"word,omitempty"`         // 词边界匹配（原 -w）
	FilesOnly  bool     `json:"files_only,omitempty"`   // 只输出命中文件路径（原 -l）
	Count      bool     `json:"count,omitempty"`        // 只输出每文件命中数（原 -c）
	MaxPerFile int      `json:"max_per_file,omitempty"` // 每文件命中上限（原 -m）

	// cp / mv
	Src string `json:"src,omitempty"`
	Dst string `json:"dst,omitempty"`

	// cp（目录）/ rm（非空目录）
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
	case "ls":
		return fsLs(ctx, &env2, &p)
	case "rg":
		return fsRg(ctx, &env2, &p)
	case "cp":
		return fsCp(ctx, &env2, &p)
	case "mv":
		return fsMv(ctx, &env2, &p)
	case "rm":
		return fsRm(ctx, &env2, &p)
	}
	return nil, &proto.ExecError{Tool: proto.ToolFS,
		Reason: fmt.Sprintf("unknown action %q (supported: read, write, edit, ls, rg, cp, mv, rm)", p.Action)}
}
