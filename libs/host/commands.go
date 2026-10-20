package host

import (
	"context"
	"fmt"
	"github.com/veypi/aic-pod/protocol"
	"path/filepath"

	"github.com/veypi/aic-pod/libs/execution"
	"github.com/veypi/aic-pod/libs/hostfs"

	"github.com/veypi/vbox"
	"github.com/veypi/vsh/commands"
)

// fsDecision 是 fs 数据面与 fsx 共用的规则表判定入口：快照每次调用取当次
// 值（grant temp 动态行即时生效；first-wins 行序）。noFollow = unlink/rename
// 语义（作用于链接本身）。
func (c *Client) fsDecision(sid, abs string, write, noFollow bool) vbox.Decision {
	op := vbox.OpRead
	if write {
		op = vbox.OpWrite
	}
	rules := c.perms.fsSnapshot(sid)
	if noFollow {
		return rules.MatchNoFollow(abs, op)
	}
	return rules.Match(abs, op)
}

// fsGate 返回 fsx 的 vbox 规则表门：读默认开放；写出区硬拒，报错引导 grant fs
// （与引擎 fs_host 适配器同一 matcher、同一份策略源）。
func (c *Client) fsGate(sid string) func(op, abs string, write bool) error {
	return func(op, abs string, write bool) error {
		if d := c.fsDecision(sid, abs, write, false); !d.Allow {
			return fmt.Errorf("%s: %s 被规则表拒绝（越界硬拒绝；如需写入请用 exec 执行 `grant fs %s`）", op, abs, abs)
		}
		return nil
	}
}

// preflightSSHLocal 是 scp/sftp 本地路径的 fs 域预检（sshx.Deps.CheckLocal）。
func (c *Client) preflightSSHLocal(ctx context.Context, inv *commands.Invocation, name string, write bool) error {
	op := vbox.OpRead
	if write {
		op = vbox.OpWrite
	}
	abs := inv.FS.Resolve(name)
	if !c.perms.fsSnapshot(execution.SessionFromContext(ctx)).Match(abs, op).Allow {
		return fmt.Errorf("%w: request grant fs %s", execution.ErrRuleDenied, abs)
	}
	return nil
}

func (c *Client) initFilesystem() error {
	store, err := hostfs.NewBytes(hostfs.BytesConfig{MaxSources: c.options().Transfers.MaxSources, MaxSourceBytes: c.options().Transfers.MaxUploadBytes})
	if err != nil {
		return err
	}
	roots, home, osHome, err := deviceFileRoots(c.options().WorkDir)
	if err != nil {
		store.Close()
		return err
	}
	files, err := hostfs.New(hostfs.Config{Roots: roots, Home: &home, OSHome: osHomePtr(osHome), Bytes: store, MaxProxyUploadBytes: c.options().Transfers.ProxyUploadBytes, Check: func(ctx context.Context, call hostfs.Call, path string, write bool) error {
		if c.perms.err() != nil {
			return protocol.Fail("permission_denied", "Device authorization configuration is invalid; repair local settings")
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		p := filepath.ToSlash(path)
		// remove/move 是 unlink/rename 语义：末段符号链接不跟随（删/挪的是链接
		// 本身）——否则可写根内的外向链接会被目标路径的策略拒绝（2026-09-22：
		// fs rm 删 venv 被 .venv/bin/python -> /opt/homebrew/... 挡下）。
		noFollow := call.Method == "remove" || call.Method == "move"
		if d := c.fsDecision(call.Caller.Origin, p, write, noFollow); !d.Allow {
			return protocol.Fail("permission_denied", fmt.Sprintf("fs: %s 被规则表拒绝（如需写入请 grant fs %s）", p, p))
		}
		return nil
	}})
	if err != nil {
		store.Close()
		return err
	}
	c.files = files
	c.bytes = store
	// text.{action}（fsx 薄层 + vbox 规则表门）由 tools.go handleFS 直接分发——
	// FS 数据面直调（§4.4），不再经方法目录注册。
	return nil
}
