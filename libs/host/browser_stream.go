package host

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/veypi/aic-pod/cfg"
	"github.com/veypi/aic-pod/libs/mcpx"
	rtcwire "github.com/veypi/aic-pod/protocol/hosts_rtc"
	wire "github.com/veypi/aic-pod/protocol/tool"
)

// The built-in UI attaches only to our default agent-browser daemon. Owner MCP
// overrides are independent services, with their own UI/transport contracts.
func (c *Client) streamBrowser(parent context.Context, caller wire.Caller, input <-chan []byte, send func([]byte) error) error {
	check := func() error {
		if err := caller.Validate(parent); err != nil {
			return err
		}
		if !caller.Direct || caller.Scope != "" || cfg.CheckAuth() != nil || !c.execAllowed(caller.Origin, "mcp.browser") {
			return wire.Fail("permission_denied", "Browser access denied; request grant cmd mcp.browser")
		}
		return nil
	}
	if err := check(); err != nil {
		return err
	}
	c.mcpMu.RLock()
	config, lifetime := c.browserConfig, c.browserContext
	c.mcpMu.RUnlock()
	if config == nil || lifetime == nil {
		return wire.Fail("unsupported", "Live view requires the built-in agent-browser service")
	}
	// agent-browser publishes its OS-assigned stream port beside the daemon socket.
	raw, err := os.ReadFile(filepath.Join(config.Env["AGENT_BROWSER_SOCKET_DIR"], "aic.stream"))
	if err != nil {
		return fmt.Errorf("Browser stream is unavailable; open the browser first: %w", err)
	}
	port, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || port < 1 || port > 65535 {
		return fmt.Errorf("Invalid agent-browser stream port")
	}
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	stop := context.AfterFunc(lifetime, cancel)
	defer stop()
	ws, _, err := websocket.Dial(ctx, fmt.Sprintf("ws://127.0.0.1:%d/?pacing=ack&maxFps=15", port), nil)
	if err != nil {
		return fmt.Errorf("Browser stream: %w", err)
	}
	defer ws.CloseNow()
	ws.SetReadLimit(rtcwire.BrowserMessageLimit)
	writes := make(chan error, 1)
	go func() {
		var err error
		defer func() { writes <- err; cancel() }()
		for {
			select {
			case <-ctx.Done():
				return
			case raw := <-input:
				if err = check(); err != nil {
					return
				}
				if err = ws.Write(ctx, websocket.MessageText, raw); err != nil {
					return
				}
			}
		}
	}()
	for {
		_, raw, err := ws.Read(ctx)
		if err != nil {
			select {
			case e := <-writes:
				if e != nil {
					return e
				}
			default:
			}
			return err
		}
		if err = check(); err != nil {
			return err
		}
		if err = send(raw); err != nil {
			return err
		}
	}
}

// The upstream MCP frontend leaves its daemon alive. Close only AIC's isolated
// daemon directory, using the upstream CLI; never inspect/kill system browsers.
func (c *Client) closeBrowser(config *mcpx.Config) {
	if config == nil {
		return
	}
	dir := config.Env["AGENT_BROWSER_SOCKET_DIR"]
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, config.Command, "close", "--all")
	cmd.Dir = config.Cwd
	cmd.Env = os.Environ()
	for k, v := range config.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		c.logf("agent-browser close: %v: %.500s", err, out)
	}
}
