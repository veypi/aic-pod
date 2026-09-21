package host

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/veypi/aic-pod/libs/hostauth"
	"github.com/veypi/aic-pod/libs/hostfs"
	tool "github.com/veypi/aic-pod/libs/hosts_tool"
	"github.com/veypi/aic-pod/libs/vcore"
	rtcwire "github.com/veypi/aic-pod/protocol/hosts_rtc"
	wire "github.com/veypi/aic-pod/protocol/hosts_tools"
	"path/filepath"
	"strconv"
	"strings"
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
	key, err := rtcwire.DirectKey(parts[2], parts[0])
	if err != nil {
		return nil, err
	}
	return hostauth.NewAccess(hostauth.AccessConfig{HostID: parts[0], UserID: parts[3], CredentialVersion: version, Key: key})
}
func (c *Client) initFilesystem() error {
	store, err := hostfs.NewBytes(hostfs.BytesConfig{MaxSources: c.options().Transfers.MaxSources, MaxSourceBytes: c.options().Transfers.MaxUploadBytes})
	if err != nil {
		return err
	}
	roots, home, err := deviceFileRoots(c.options().WorkDir)
	if err != nil {
		store.Close()
		return err
	}
	files, err := hostfs.New(hostfs.Config{Roots: roots, Home: &home, Bytes: store, MaxProxyUploadBytes: c.options().Transfers.ProxyUploadBytes, Check: func(ctx context.Context, call hostfs.Call, path string, write bool) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		env := c.newEnv(call.Caller.Origin, "")
		env.Granted = call.Caller.Level
		if err := env.CheckPath("fs", filepath.ToSlash(path)); err != nil {
			return wire.Fail("permission_denied", err.Error())
		}
		if err := env.CheckPolicy("fs", filepath.ToSlash(path), write); err != nil {
			return wire.Fail("permission_denied", err.Error())
		}
		return nil
	}})
	if err != nil {
		store.Close()
		return err
	}
	c.files = files
	c.bytes = store
	methods := files.Methods()
	// Text-oriented AI operations use this filesystem's same version-aware VFS.
	for _, action := range vcore.FSActions {
		methods = append(methods, tool.Method{Descriptor: wire.Method{Name: "text." + action, Mode: wire.Call, Access: 1, Input: json.RawMessage(`{"type":"object"}`)}, Run: func(ctx context.Context, caller tool.Caller, args json.RawMessage) (any, error) {
			var params map[string]any
			if err := wire.Decode(args, &params); err != nil {
				return nil, err
			}
			params["action"] = action
			raw, _ := json.Marshal(params)
			env := c.newEnv(caller.Origin, "")
			env.Granted = caller.Level
			env.VFS = files.View(ctx, caller)
			if required := vcore.FSRequiredIn(env, action, raw); caller.Level < required {
				return nil, wire.Fail("permission_denied", "Insufficient filesystem grant")
			}
			result, err := vcore.RunFS(ctx, env, raw)
			if result != nil {
				c.attachFileURL(result.Attrs)
			}
			return result, err
		}})
	}
	return c.tools.RegisterFS(methods...)
}
