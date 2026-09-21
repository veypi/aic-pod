package host

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"github.com/veypi/aic-pod/libs/exec_procs"
	tool "github.com/veypi/aic-pod/libs/hosts_tool"
	"github.com/veypi/aic-pod/libs/proto"
	"github.com/veypi/aic-pod/libs/vcore"
	wire "github.com/veypi/aic-pod/protocol/hosts_tools"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type commandArgs struct {
	Argv      []string `json:"argv"`
	Workdir   string   `json:"workdir,omitempty"`
	NoSandbox bool     `json:"nosandbox,omitempty"`
}

func (c *Client) registerCommands() {
	for _, decl := range commandDefinitions() {
		name := decl.Name
		managed := name != "commands" && name != "bg_list" && name != "bg_wait" && name != "bg_kill" && name != "grant"
		method := tool.Bind(tool.Spec{Name: "run", Access: decl.RequiredLevel, Background: managed}, func(ctx context.Context, caller tool.Caller, a commandArgs) (any, error) {
			data, _ := json.Marshal(map[string]any{"action": name, "argv": a.Argv, "workdir": a.Workdir, "nosandbox": a.NoSandbox})
			req := &proto.ToolRequest{Tool: proto.ToolExec, MsgID: caller.RequestID, SessionID: caller.Origin, GrantedLevel: caller.Level, Data: data}
			if strings.HasPrefix(name, "bg_") {
				return c.executionControl(ctx, caller, name, a.Argv)
			}
			response := c.execCmd(ctx, caller.Origin, req)
			if response.Error != "" {
				return nil, wire.Fail(string(response.State), response.Error)
			}
			return &vcore.Result{Content: response.Content, Attrs: response.Attrs}, nil
		})
		method.Required = func(raw json.RawMessage) int {
			var a commandArgs
			_ = json.Unmarshal(raw, &a)
			level := vcore.ExecRequired(name, a.Argv)
			if a.NoSandbox {
				level = max(level, 4)
			}
			return level
		}
		command := tool.Command{Name: name, Desc: decl.Desc, Help: decl.Help, Access: decl.RequiredLevel, RawArgv: true, Methods: []tool.Method{method}}
		if err := c.tools.RegisterCommand(command); err != nil {
			panic(err)
		}
	}
}
func executionOwner(c tool.Caller) string { return c.Subject + "\x00" + c.Origin }
func (c *Client) executeCommand(ctx context.Context, caller tool.Caller, r wire.Request, in wire.Invocation, m tool.Method) (any, error) {
	if r.Execution != nil && r.Execution.Epoch != c.procs.Epoch() {
		return nil, wire.Fail("expired", "Execution belongs to a different device runtime")
	}
	if r.Execution == nil && !m.Descriptor.Background {
		return m.Run(ctx, caller, in.Args)
	}
	if r.Execution != nil && r.Execution.WaitMS != nil && !m.Descriptor.Background {
		return nil, wire.Fail("unsupported", "Method does not support background execution")
	}
	id := r.ID
	waitMS := int64(30000)
	output := ""
	if r.Execution != nil {
		id = r.Execution.ID
		output = r.Execution.Output
		if r.Execution.WaitMS != nil {
			waitMS = *r.Execution.WaitMS
		}
	}
	owner := executionOwner(caller)
	hash := sha256.Sum256([]byte(owner))
	// Ownership is independent of RTC connections and is safe as a path segment.
	namespace := fmt.Sprintf("%x", hash[:16])
	fullID := c.hostID + ":" + namespace + ":" + id
	if output == "" {
		output = filepath.Join(c.sessionWorkDir(namespace), ".exec", c.procs.Epoch(), id+".log")
	} else {
		env := c.newEnv(caller.Origin, "")
		env.Granted = caller.Level
		output = expandHomeDir(output)
		if !filepath.IsAbs(output) {
			output = filepath.Join(c.options().WorkDir, output)
		}
		if err := env.CheckPath("exec", output); err != nil {
			return nil, err
		}
		if err := env.CheckPolicy("exec", output, true); err != nil {
			return nil, err
		}
	}
	// Capture this admission's authority. A new connection never extends it.
	runCaller := caller
	runCaller.Expiry = nil
	runCaller.Check = nil
	if caller.AllowStreams {
		runCaller.ExpiresAt = time.Now().Add(c.options().ExecTimeout)
	}
	runCaller.Check = func(run context.Context) error {
		if !c.execAllowed(runCaller.Origin, in.Command) {
			return wire.Fail("permission_denied", "Command authorization revoked")
		}
		return nil
	}
	digestRaw, _ := json.Marshal(struct {
		Call    wire.Invocation
		Timeout int64
		Output  string
		Level   int
	}{in, r.TimeoutMS, output, caller.Level})
	digest := sha256.Sum256(digestRaw)
	wait, cancel := context.WithTimeout(ctx, time.Duration(waitMS)*time.Millisecond)
	defer cancel()
	required := m.Descriptor.Access
	if m.Required != nil {
		required = max(required, m.Required(in.Args))
	}
	if r.Execution != nil && r.Execution.Output != "" {
		required = max(required, 2)
	}
	res, err := c.procs.StartCall(wait, exec_procs.CallOptions{RequiredLevel: required, KeepOutput: r.Execution != nil && r.Execution.Output != "", ID: fullID, Owner: owner, Digest: fmt.Sprintf("%x", digest), Command: in.Command + " " + in.Method, LogPath: output, Timeout: time.Duration(r.TimeoutMS) * time.Millisecond, AuthorizationDeadline: runCaller.ExpiresAt, Check: runCaller.Validate, Run: func(run context.Context, out io.Writer) (any, error) {
		runCaller.Output = out
		value, err := m.Run(run, runCaller, in.Args)
		if value != nil {
			if result, ok := value.(*vcore.Result); ok {
				if result.Content != "" {
					fmt.Fprintln(out, result.Content)
				}
			} else {
				raw, _ := json.Marshal(value)
				if len(raw) < wire.MaxMessageBytes/2 {
					fmt.Fprintln(out, string(raw))
				}
			}
		}
		return value, err
	}})
	if err != nil {
		return nil, err
	}
	if !m.Descriptor.Background && res.Background {
		_ = c.procs.Kill(fullID)
	}
	return res, nil
}
func (c *Client) executionControl(ctx context.Context, caller tool.Caller, name string, argv []string) (any, error) {
	owner := executionOwner(caller)
	if name == "bg_list" {
		items := []map[string]any{}
		for _, e := range c.procs.List() {
			if e.Owner == owner && caller.Level >= e.RequiredLevel && c.execAllowed(caller.Origin, strings.Fields(e.Command)[0]) {
				items = append(items, map[string]any{"id": e.ID, "command": e.Command, "status": e.Status(), "output": e.LogPath, "started": e.Started, "pid": e.PID()})
			}
		}
		return items, nil
	}
	if len(argv) == 0 {
		return nil, wire.Fail("invalid_argument", "Execution ID required")
	}
	e := c.procs.Get(argv[0])
	if e == nil || e.Owner != owner {
		return nil, wire.Fail("not_found", "Execution unavailable or expired")
	}
	if caller.Level < e.RequiredLevel || !c.execAllowed(caller.Origin, strings.Fields(e.Command)[0]) {
		return nil, wire.Fail("permission_denied", "Execution result requires the original command permission")
	}
	if name == "bg_kill" {
		if err := c.procs.Kill(e.ID); err != nil {
			return nil, err
		}
		return map[string]any{"id": e.ID, "status": e.Status()}, nil
	}
	wait := 30 * time.Second
	if len(argv) == 3 && argv[1] == "--wait" {
		seconds, err := strconv.Atoi(argv[2])
		if err != nil || seconds < 0 || seconds > 300 {
			return nil, wire.Fail("invalid_argument", "wait must be 0..300 seconds")
		}
		wait = time.Duration(seconds) * time.Second
	} else if len(argv) != 1 {
		return nil, wire.Fail("invalid_argument", "Expected bg_wait ID [--wait SECONDS]")
	}
	return c.procs.Wait(ctx, e.ID, wait)
}
