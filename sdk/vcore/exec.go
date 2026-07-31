package vcore

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/veypi/aic-pod/sdk/proto"
)

// CmdFunc 是虚拟指令实现：action + argv → Result。
type CmdFunc func(ctx context.Context, env *Env, argv []string) (*Result, error)

// coreCommands 是核心 8 虚拟指令（§5.4，三端统一基准）。
var coreCommands = map[string]CmdFunc{
	"ls":    cmdLs,
	"find":  cmdFind,
	"grep":  cmdGrep,
	"curl":  cmdCurl,
	"rm":    cmdRm,
	"mkdir": cmdMkdir,
	"cp":    cmdCp,
	"mv":    cmdMv,
}

// CoreCommandNames 返回核心 8 指令名（排序，供 commands 聚合与报错提示）。
func CoreCommandNames() []string {
	names := make([]string, 0, len(coreCommands))
	for n := range coreCommands {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Register 注册额外虚拟指令（git/browser/bg_*/commands 由引入方或后续阶段接入）。
// 同名覆盖核心 8 不被允许（一致性基准不可替换）。
func Register(name string, fn CmdFunc) error {
	if _, ok := coreCommands[name]; ok {
		return fmt.Errorf("vcore: cannot override core command %q", name)
	}
	extraCommands[name] = fn
	return nil
}

var extraCommands = map[string]CmdFunc{}

// Run 执行 exec 虚拟指令（§5：action = 虚拟指令名，argv 原样透传）。
// 程序命令不在 vcore 职责内——由 host 端进程执行承载。
func Run(ctx context.Context, env *Env, action string, argv []string) (*Result, error) {
	if action == "" {
		return nil, &proto.ExecError{Tool: proto.ToolExec,
			Reason: "action is required (supported: " + strings.Join(supportedNames(), ", ") + ")"}
	}
	if fn, ok := coreCommands[action]; ok {
		return fn(ctx, env, argv)
	}
	if fn, ok := extraCommands[action]; ok {
		return fn(ctx, env, argv)
	}
	return nil, &proto.ExecError{Tool: proto.ToolExec,
		Reason: fmt.Sprintf("unknown action %q (supported: %s)", action, strings.Join(supportedNames(), ", "))}
}

func supportedNames() []string {
	names := CoreCommandNames()
	for n := range extraCommands {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
