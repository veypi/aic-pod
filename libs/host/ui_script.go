package host

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/veypi/aic-pod/libs/exec_procs"
	"github.com/veypi/aic-pod/libs/proto"
	"github.com/veypi/aic-pod/libs/uiscript"
	"github.com/veypi/aic-pod/protocol/ui"
)

// A script is an orchestrator, not a new provider. Each step re-enters execCmd
// with the same domain/session/grant and independently checked file policy.
// The outer request journal owns retries; no script or partial run is replayed.
var uiScriptSlots = make(chan struct{}, 4)

type uiScriptStep struct {
	Index      int        `json:"index"`
	Op         string     `json:"op"`
	DurationMS int64      `json:"duration_ms"`
	Result     *ui.Result `json:"result"`
}

func (c *Client) runUIScript(ctx context.Context, sid string, req *proto.ToolRequest, o *ui.Operation, workdir string) *proto.ToolResponse {
	r := ui.NewResult(o)
	steps := []uiScriptStep{}
	logs := []json.RawMessage{}
	data := map[string]any{"steps": steps, "return": nil, "logs": logs, "steps_completed": 0}
	r.Data = data
	performed := any(false)
	finish := func(err error) *proto.ToolResponse {
		data["steps"], data["logs"] = steps, logs
		if err != nil {
			r.Fail(err, performed)
		} else {
			r.Action = map[string]any{"performed": performed}
		}
		return uiResponse(req.MsgID, o, r, c.uiWorkDir(sid))
	}
	rejected := func(err error) *proto.ToolResponse {
		r.Fail(err, false)
		r.State = "rejected"
		return uiResponse(req.MsgID, o, r, c.uiWorkDir(sid))
	}
	if req.GrantedLevel < o.Level() {
		return rejected(ui.Err("permission_denied", "run requires level 3"))
	}
	ctx, cancel := context.WithTimeout(ctx, o.Timeout())
	defer cancel()
	if deadline, err := time.Parse(time.RFC3339Nano, req.Deadline); err == nil {
		var stop context.CancelFunc
		ctx, stop = context.WithDeadline(ctx, deadline)
		defer stop()
	}
	code := o.String("code")
	if source := o.String("file"); source != "" {
		env := c.newEnv(sid, workdir)
		env.Granted = req.GrantedLevel
		file, err := env.Resolve(source)
		if err == nil {
			err = env.CheckPath(o.Domain+" run", file)
		}
		if err == nil {
			err = env.CheckPolicy(o.Domain+" run", file, false)
		}
		if err != nil {
			return rejected(ui.Err("permission_denied", err.Error()))
		}
		info, err := os.Stat(file)
		if err != nil {
			return finish(err)
		}
		if !info.Mode().IsRegular() {
			return finish(ui.Err("invalid_argument", "script must be a regular file"))
		}
		f, err := os.Open(file)
		if err != nil {
			return finish(err)
		}
		info, err = f.Stat()
		if err != nil || !info.Mode().IsRegular() {
			f.Close()
			return finish(ui.Err("invalid_argument", "script must be a regular file"))
		}
		body, err := io.ReadAll(io.LimitReader(f, uiscript.MaxCode+1))
		f.Close()
		if err != nil {
			return finish(err)
		}
		code = string(body)
	}
	if strings.TrimSpace(code) == "" || len(code) > uiscript.MaxCode {
		return finish(ui.Err("invalid_argument", "script must contain 1..512 KiB of JavaScript"))
	}
	select {
	case uiScriptSlots <- struct{}{}:
		defer func() { <-uiScriptSlots }()
	case <-ctx.Done():
		return finish(ui.Err("timeout", ctx.Err().Error()))
	}
	executable, err := os.Executable()
	if err != nil {
		return finish(err)
	}
	argv := []string{executable, uiscript.WorkerArg}
	if len(c.uiScriptExec) > 0 {
		argv = append([]string{}, c.uiScriptExec...)
	}
	// Scripts have no direct OS bindings. Still apply the same host filesystem
	// deny rules and an OS read-only/network-disabled profile to the worker.
	manager := exec_procs.NewManager(o.Timeout())
	process, err := manager.Spawn(ctx, exec_procs.StartOptions{Exec: argv, Level: proto.LevelRead, DenyPaths: c.policy.DenyPatterns(), ReadPaths: c.policy.ReadPatternsFor(sid), FsOpen: c.policy.OpenMode(), NetOpen: false})
	if err != nil {
		return finish(ui.Err("script_unavailable", err.Error()))
	}
	defer process.Abort()
	defer process.Stdin.Close()
	encoder := json.NewEncoder(process.Stdin)
	if err := encoder.Encode(uiscript.Init{Domain: o.Domain, Code: code}); err != nil {
		return finish(err)
	}
	scanner := bufio.NewScanner(process.Body())
	scanner.Buffer(make([]byte, 4096), uiscript.MaxMessage)
	currentTarget := o.Target
	effects := map[string]any{}
	call := func(argv []string) *ui.Result {
		failed := func(op string, err error) *ui.Result {
			result := ui.NewResult(&ui.Operation{Domain: o.Domain, Op: op})
			result.Fail(err, false)
			return result
		}
		if err := ctx.Err(); err != nil {
			return failed("unknown", ui.Err("timeout", err.Error()))
		}
		step, err := ui.Parse(o.Domain, argv)
		if err != nil {
			return failed("unknown", err)
		}
		if step.Op == "run" {
			return failed("run", ui.Err("unsupported", "nested run is not supported"))
		}
		// Canonical flags are prepended so argv's optional -- terminator remains
		// intact. Replace format rather than allowing a duplicate flag.
		args := uiScriptFormat(argv)
		if step.Target == "" && currentTarget != "" && !ui.Has([]string{"open", "target.use", "target.list", "help", "capabilities", "apps", "doctor"}, step.Op) {
			args = uiScriptOption(args, "target", currentTarget)
		}
		child := *req
		child.Tool = proto.ToolExec
		child.MsgID = fmt.Sprintf("%s:step:%d", req.MsgID, len(steps)+1)
		if dl, ok := ctx.Deadline(); ok {
			child.Deadline = dl.UTC().Format(time.RFC3339Nano)
		}
		child.Data, _ = json.Marshal(map[string]any{"action": o.Domain, "argv": args, "workdir": workdir})
		if state, message := c.checkGranted(&child); state != "" {
			result := failed(step.Op, ui.Err("permission_denied", message))
			result.State = string(state)
			return result
		}
		response := c.execCmd(ctx, sid, &child)
		normalizeUIResponse(response, o.Domain, args)
		var result ui.Result
		if err := json.Unmarshal([]byte(response.Content), &result); err != nil || result.Protocol != "ui/1" || result.Domain != o.Domain || result.State != string(response.State) {
			result = *failed(step.Op, ui.Err("invalid_response", "UI step did not return ui/1 JSON"))
			result.Action = map[string]any{"performed": "unknown"}
		}
		result.Images = response.Attrs
		if result.Target != nil {
			r.Target = result.Target
		}
		if result.State == "completed" && result.Target != nil && (currentTarget == "" && step.Target == "" || step.Op == "target.use" || step.Op == "open") {
			currentTarget = str(result.Target["id"])
		}
		if result.State == "completed" && step.Op == "close" && result.Target != nil && str(result.Target["id"]) == currentTarget {
			currentTarget = ""
		}
		if effect := result.Action["performed"]; effect != nil {
			effects[child.MsgID] = effect
		}
		if data, ok := result.Data.(map[string]any); ok && ui.Has([]string{"dialog.accept", "dialog.dismiss"}, step.Op) {
			if resumed, ok := data["resumed"].(map[string]any); ok {
				id := str(resumed["request_id"])
				if _, exists := effects[id]; exists {
					if action, ok := resumed["action"].(map[string]any); ok {
						if effect := action["performed"]; effect == true || effect == false {
							effects[id] = effect
						}
					}
				}
			}
		}
		performed = false
		for _, effect := range effects {
			if effect == "unknown" {
				performed = "unknown"
				break
			}
			if effect == true {
				performed = true
			}
		}
		return &result
	}
	completed, logBytes, totalBytes := 0, 0, 0
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return finish(ui.Err("timeout", err.Error()))
		}
		var message uiscript.Message
		if err := json.Unmarshal(scanner.Bytes(), &message); err != nil {
			return finish(ui.Err("script_protocol", err.Error()))
		}
		switch message.Kind {
		case "log":
			logBytes += len(message.Value)
			if logBytes > 64*1024 {
				return finish(ui.Err("resource_limit", "script log exceeds 64 KiB"))
			}
			logs = append(logs, append(json.RawMessage{}, message.Value...))
		case "call":
			if len(steps) >= uiscript.MaxSteps {
				data["failed_step"] = len(steps) + 1
				return finish(ui.Err("resource_limit", "script exceeds 256 UI steps"))
			}
			started := time.Now()
			result := call(message.Argv)
			// Intermediate images are artifacts; only --after screenshot is inlined.
			result.Images = nil
			steps = append(steps, uiScriptStep{Index: len(steps) + 1, Op: result.Op, DurationMS: time.Since(started).Milliseconds(), Result: result})
			if result.State == "completed" {
				completed++
				data["steps_completed"] = completed
			}
			if err := ctx.Err(); err != nil {
				if result.State != "completed" {
					data["failed_step"] = len(steps)
				}
				return finish(ui.Err("timeout", err.Error()))
			}
			reply, err := json.Marshal(map[string]any{"index": len(steps), "result": result})
			if err != nil {
				return finish(err)
			}
			totalBytes += len(reply)
			if totalBytes > 8*1024*1024 {
				return finish(ui.Err("resource_limit", "script step results exceed 8 MiB"))
			}
			if len(reply) >= uiscript.MaxMessage {
				return finish(ui.Err("resource_limit", "UI step result exceeds bridge limit"))
			}
			if _, err = process.Stdin.Write(append(reply, '\n')); err != nil {
				if ctx.Err() != nil {
					return finish(ui.Err("timeout", ctx.Err().Error()))
				}
				return finish(ui.Err("script_failed", err.Error()))
			}
		case "done":
			if message.Step > 0 && message.Step <= len(steps) {
				data["failed_step"] = message.Step
			}
			if message.Error != nil {
				return finish(message.Error)
			}
			if len(message.Value) > 0 {
				data["return"] = message.Value
			}
			if o.Options.After != "none" && currentTarget != "" {
				operation := "snapshot"
				if o.Options.After == "screenshot" {
					operation = "screenshot"
				}
				after := call([]string{operation, "--target", currentTarget})
				if after.State == "completed" {
					r.Observation = after.Observation
					r.Artifacts = after.Artifacts
					if o.Options.After == "screenshot" {
						r.Images = after.Images
					}
				} else {
					message := "final observation did not complete"
					if after.Error != nil {
						message = after.Error.Message
					}
					r.Warn("observation_failed", message)
				}
			}
			return finish(nil)
		default:
			return finish(ui.Err("script_protocol", "unknown worker message"))
		}
	}
	if ctx.Err() != nil {
		return finish(ui.Err("timeout", ctx.Err().Error()))
	}
	if err := scanner.Err(); err != nil && strings.Contains(err.Error(), "ui-script memory limit exceeded") {
		return finish(ui.Err("resource_limit", "script worker exceeded its memory limit"))
	}
	return finish(ui.Err("script_failed", fmt.Sprintf("script worker ended without a result: %v", scanner.Err())))
}

func uiScriptOption(argv []string, key, value string) []string {
	n := 1
	if len(argv) > 1 && !strings.HasPrefix(argv[1], "--") && strings.Contains(argv[0], ".") == false {
		if _, ok := ui.Schema.Commands[argv[0]+"."+argv[1]]; ok {
			n = 2
		}
	}
	result := append([]string{}, argv[:n]...)
	result = append(result, "--"+key, value)
	return append(result, argv[n:]...)
}
func uiScriptFormat(argv []string) []string {
	var result []string
	literal := false
	for i := 0; i < len(argv); i++ {
		if argv[i] == "--" {
			literal = true
		}
		if !literal && argv[i] == "--format" {
			i++
			continue
		}
		result = append(result, argv[i])
	}
	return uiScriptOption(result, "format", "json")
}
