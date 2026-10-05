package host

import (
	"context"
	"fmt"
	"github.com/veypi/aic-pod/protocol"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/veypi/aic-pod/cfg"
	"github.com/veypi/aic-pod/libs/hostauth"
	"github.com/veypi/aic-pod/libs/hostfs"

	"github.com/veypi/vbox"
)

func (c *Client) newAccess() (*hostauth.Access, error) {
	parts := strings.SplitN(c.options().Key, ".", 4)
	if len(parts) != 4 {
		return nil, fmt.Errorf("invalid device credential")
	}
	version, err := strconv.ParseUint(parts[1], 10, 64)
	if err != nil {
		return nil, err
	}
	key, err := protocol.RtcDirectKey(parts[2], parts[0])
	if err != nil {
		return nil, err
	}
	return hostauth.NewAccess(hostauth.AccessConfig{HostID: parts[0], UserID: parts[3], CredentialVersion: version, Key: key, Now: clockNow})
}

// fsGate 返回 fsx 的 vbox 规则表门（fsauth 快照源每次调用取当次值——grant
// temp 动态行即时生效；first-wins 行序）。读默认开放；写出区硬拒，报错
// 引导 grant fs（与引擎 fs_host 适配器同一 matcher、同一份策略源）。
func (c *Client) fsGate(sid string) func(op, abs string, write bool) error {
	return func(op, abs string, write bool) error {
		vop := vbox.OpRead
		if write {
			vop = vbox.OpWrite
		}
		if d := c.policy.Snapshot(sid).Match(abs, vop); !d.Allow {
			return fmt.Errorf("%s: %s 被规则表拒绝（越界硬拒绝；如需写入请用 exec 执行 `grant fs %s`）", op, abs, abs)
		}
		return nil
	}
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
		if cfg.CheckAuth() != nil {
			return protocol.Fail("permission_denied", "Device authorization configuration is invalid; repair local settings")
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		p := filepath.ToSlash(path)
		vop := vbox.OpRead
		if write {
			vop = vbox.OpWrite
		}
		rules := c.policy.Snapshot(call.Caller.Origin)
		// remove/move 是 unlink/rename 语义：末段符号链接不跟随（删/挪的是链接
		// 本身）——否则可写根内的外向链接会被目标路径的策略拒绝（2026-09-22：
		// fs rm 删 venv 被 .venv/bin/python -> /opt/homebrew/... 挡下）。
		var d vbox.Decision
		if call.Method == "remove" || call.Method == "move" {
			d = rules.MatchNoFollow(p, vop)
		} else {
			d = rules.Match(p, vop)
		}
		if !d.Allow {
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
