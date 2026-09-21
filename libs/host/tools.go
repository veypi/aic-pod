package host

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"github.com/nats-io/nats.go"
	"github.com/veypi/aic-pod/cfg"
	"github.com/veypi/aic-pod/libs/browser"
	"github.com/veypi/aic-pod/libs/cua"
	tool "github.com/veypi/aic-pod/libs/hosts_tool"
	natswire "github.com/veypi/aic-pod/protocol/hosts_nats"
	wire "github.com/veypi/aic-pod/protocol/hosts_tools"
	"path/filepath"
	"strings"
	"time"
)

func (c *Client) initTools() {
	c.tools = tool.New(tool.Config{ExecutionEpoch: c.procs.Epoch(), Execute: c.executeCommand, Authorize: func(ctx context.Context, caller tool.Caller, name string, m wire.Method) error {
		if caller.Subject != "catalog" && name != "fs" && !c.execAllowed(caller.Origin, name) {
			return wire.Fail("permission_denied", "Tool is denied by device policy")
		}
		return nil
	}})
	c.registerCommands()
	c.initErr = c.initFilesystem()
	root := c.options().BrowserStateDir
	if root == "" {
		if dir, err := cfg.StateDir(); err == nil {
			parts := strings.SplitN(c.options().Key, ".", 4)
			binding := "unbound"
			if len(parts) == 4 {
				binding = parts[0] + "\x00" + parts[3]
			}
			digest := sha256.Sum256([]byte(binding))
			root = filepath.Join(dir, "browser", fmt.Sprintf("%x", digest[:16]))
		}
	}
	c.browser = browser.New(browser.Config{Path: c.options().BrowserPath, StateDir: root, Width: c.options().BrowserWidth, Height: c.options().BrowserHeight, CheckFile: func(ctx context.Context, caller tool.Caller, path string, write bool) error {
		if err := caller.Validate(ctx); err != nil {
			return err
		}
		env := c.newEnv(caller.Origin, "")
		env.Granted = caller.Level
		if err := env.CheckPath("browser", filepath.ToSlash(path)); err != nil {
			return err
		}
		return env.CheckPolicy("browser", filepath.ToSlash(path), write)
	}})
	native := cua.New(cua.Config{Logf: c.logf})
	if err := c.tools.RegisterCommand(native.Tool()); err != nil {
		panic(err)
	}
	if err := c.tools.RegisterCommand(c.browser.Tool()); err != nil {
		panic(err)
	}
}
func (c *Client) HandleTool(ctx context.Context, caller tool.Caller, r wire.Request) wire.Response {
	return c.tools.Handle(ctx, caller, r)
}
func (c *Client) OpenToolStream(ctx context.Context, caller tool.Caller, in wire.Invocation) (tool.Stream, error) {
	return c.tools.OpenStream(ctx, caller, in)
}
func (c *Client) DisconnectTools(caller tool.Caller) { c.tools.Disconnect(caller) }
func (c *Client) HandleNATS(ctx context.Context, subject string, data []byte) wire.Response {
	var r natswire.Request
	if err := wire.Decode(data, &r); err != nil {
		return wire.Reply(natswire.Protocol, "", nil, err)
	}
	destination, err := natswire.Subject(c.uid, c.hostID)
	if err != nil || subject != destination {
		return wire.Reply(natswire.Protocol, r.Request.ID, nil, wire.Fail("unauthorized", "Wrong tool route"))
	}
	if err = natswire.Verify(c.kTool, c.hostID, subject, r, time.Now()); err != nil {
		return wire.Reply(natswire.Protocol, r.Request.ID, nil, err)
	}
	if r.Caller != c.uid {
		return wire.Reply(natswire.Protocol, r.Request.ID, nil, wire.Fail("permission_denied", "Caller does not own this device"))
	}
	deadline := time.UnixMilli(r.Deadline)
	if !c.replay.checkAndMark("tools:"+r.Nonce, deadline) {
		return wire.Reply(natswire.Protocol, r.Request.ID, nil, wire.Fail("unauthorized", "Duplicate nonce"))
	}
	// Origin is signed source metadata for existing policy grants, not a tool session.
	caller := tool.Caller{Subject: r.Caller, ConnectionID: "nats:" + r.Caller, Origin: r.Origin, Level: r.GrantedLevel, ExpiresAt: time.UnixMilli(r.AuthorizationUntil), Scope: r.Scope}
	return c.tools.Handle(ctx, caller, r.Request)
}
func (c *Client) handleToolsMsg(msg *nats.Msg) {
	response := c.HandleNATS(context.Background(), msg.Subject, msg.Data)
	data, err := json.Marshal(response)
	if err != nil {
		return
	}
	_ = msg.Respond(data)
}
