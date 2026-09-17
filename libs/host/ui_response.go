package host

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/veypi/aic-pod/libs/proto"
	"github.com/veypi/aic-pod/protocol/ui"
)

func uiFailure(msgID, domain string, argv []string, err error, state string) *proto.ToolResponse {
	o, e := ui.Parse(domain, argv)
	if e != nil {
		o = &ui.Operation{Domain: domain, Op: "unknown", Options: ui.Options{Format: "text"}}
		if len(argv) > 0 {
			o.Op = argv[0]
		}
		for i := 0; i+1 < len(argv); i++ {
			if argv[i] == "--format" && argv[i+1] == "json" {
				o.Options.Format = "json"
			}
		}
	}
	r := ui.NewResult(o)
	r.Fail(err, false)
	r.State = state
	return &proto.ToolResponse{MsgID: msgID, State: proto.State(state), Content: r.Render(o.Options.Format), Error: err.Error()}
}
func trimUI(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
func uiResponse(msgID string, o *ui.Operation, r *ui.Result, root string) *proto.ToolResponse {
	compactData := func(file string) map[string]any {
		out := map[string]any{"truncated": true}
		if file != "" {
			out["path"] = file
		}
		if o.Op == "run" {
			if data, ok := r.Data.(map[string]any); ok {
				for _, key := range []string{"steps_completed", "failed_step"} {
					if value, exists := data[key]; exists {
						out[key] = value
					}
				}
				if value, exists := data["return"]; exists {
					if encoded, err := json.Marshal(value); err == nil && len(encoded) <= 2048 {
						out["return"] = value
					}
				}
				if file == "" {
					if path, exists := data["path"]; exists {
						out["path"] = path
					}
				}
			}
		}
		return out
	}
	raw, _ := json.Marshal(r)
	if len(raw) > 12*1024 {
		dir := filepath.Join(root, ".ui")
		file := filepath.Join(dir, uiID("result-")+".json")
		if err := os.MkdirAll(dir, 0700); err == nil {
			if err = os.WriteFile(file, raw, 0600); err == nil {
				r.Artifacts = append(r.Artifacts, map[string]any{"kind": "result", "path": file, "bytes": len(raw)})
				if r.Observation != nil {
					r.Observation["text"] = trimUI(str(r.Observation["text"]), 6000)
					delete(r.Observation, "elements")
					r.Observation["truncated"] = true
				}
				if compact, _ := json.Marshal(r); len(compact) > 12*1024 {
					r.Data = compactData(file)
				}
			} else {
				r.Warn("artifact_failed", err.Error())
			}
		} else {
			r.Warn("artifact_failed", err.Error())
		}
	}
	// Keep the inline result valid and bounded even when the artifact cannot be written.
	if b, _ := json.Marshal(r); len(b) > 12*1024 {
		if r.Observation != nil {
			r.Observation["text"] = trimUI(str(r.Observation["text"]), 6000)
			delete(r.Observation, "elements")
			r.Observation["truncated"] = true
		}
		if b, _ := json.Marshal(r); len(b) > 12*1024 {
			r.Data = compactData("")
		}
	}
	if b, _ := json.Marshal(r); len(b) > 12*1024 {
		for k, v := range r.Target {
			if s, ok := v.(string); ok {
				r.Target[k] = trimUI(s, 1024)
			}
		}
		if r.Error != nil {
			r.Error.Message = trimUI(r.Error.Message, 1024)
			r.Error.Recovery = trimUI(r.Error.Recovery, 1024)
		}
		if len(r.Warnings) > 16 {
			r.Warnings = r.Warnings[:16]
		}
		for _, w := range r.Warnings {
			w["message"] = trimUI(w["message"], 512)
		}
		if b, _ := json.Marshal(r); len(b) > 12*1024 {
			r.Observation = map[string]any{"truncated": true}
			r.Data = compactData("")
		}
	}
	response := &proto.ToolResponse{MsgID: msgID, State: proto.State(r.State), Content: r.Render(o.Options.Format), Attrs: r.Images}
	if r.Error != nil {
		response.Error = r.Error.Message
	}
	return response
}
func normalizeUIResponse(resp *proto.ToolResponse, domain string, argv []string) {
	if strings.HasPrefix(resp.Content, "[ui/1]") {
		return
	}
	var r ui.Result
	if json.Unmarshal([]byte(resp.Content), &r) == nil && r.Protocol == "ui/1" {
		return
	}
	if resp.State == proto.StateCompleted {
		return
	}
	message := resp.Error
	if message == "" {
		message = "UI request failed"
	}
	n := uiFailure(resp.MsgID, domain, argv, ui.Err(map[proto.State]string{proto.StateError: "execution_failed", proto.StateRejected: "permission_denied", proto.StateWaiting: "approval_required"}[resp.State], message), string(resp.State))
	resp.Content = n.Content
	// UI results carry no business attrs. The transport state/error remain authoritative.
	for k := range resp.Attrs {
		if k != "image_data" && k != "image_path" && k != "image_compressed" {
			delete(resp.Attrs, k)
		}
	}
}

// Durable request journal: pending markers survive crashes, so an unknown outcome
// is never silently replayed. Only authenticated, authorized UI requests enter here.
var uiRequests = struct {
	sync.Mutex
	running map[string]chan struct{}
}{running: map[string]chan struct{}{}}

type uiJournal struct {
	Hash     string              `json:"hash"`
	Response *proto.ToolResponse `json:"response,omitempty"`
}

func (c *Client) uiWorkDir(sid string) string {
	if c.uiSessionRoot != "" {
		return filepath.Join(c.uiSessionRoot, sid)
	}
	return sessionWorkDir(sid)
}

func (c *Client) runUIOnce(ctx context.Context, req *proto.ToolRequest, domain string, argv []string, fn func() *proto.ToolResponse) *proto.ToolResponse {
	sid := req.SessionID
	if sid == "" || strings.ContainsAny(sid, "/\\\x00") || sid == "." || sid == ".." || req.MsgID == "" {
		return uiFailure(req.MsgID, domain, argv, ui.Err("invalid_argument", "valid session_id and msg_id required"), "error")
	}
	keyHash := sha256.Sum256([]byte(req.MsgID))
	payloadHash := sha256.Sum256(req.Data)
	digest := hex.EncodeToString(payloadHash[:])
	dir := filepath.Join(c.uiWorkDir(sid), ".ui", "requests")
	file := filepath.Join(dir, hex.EncodeToString(keyHash[:])+".json")
	uiRequests.Lock()
	if pending := uiRequests.running[file]; pending != nil {
		uiRequests.Unlock()
		select {
		case <-pending:
		case <-ctx.Done():
			return uiFailure(req.MsgID, domain, argv, ui.Err("timeout", ctx.Err().Error()), "error")
		}
		return c.runUIOnce(ctx, req, domain, argv, fn)
	}
	done := make(chan struct{})
	uiRequests.running[file] = done
	uiRequests.Unlock()
	defer func() { uiRequests.Lock(); delete(uiRequests.running, file); close(done); uiRequests.Unlock() }()
	if b, err := os.ReadFile(file); err == nil {
		var journal uiJournal
		if json.Unmarshal(b, &journal) != nil || journal.Hash != digest {
			return uiFailure(req.MsgID, domain, argv, ui.Err("request_conflict", "request ID was already used with different or invalid data"), "error")
		}
		if journal.Response != nil {
			return journal.Response
		}
		o, _ := ui.Parse(domain, argv)
		r := ui.NewResult(o)
		r.Fail(ui.Err("outcome_unknown", "previous execution did not record a terminal result; observe before issuing a new operation"), "unknown")
		return uiResponse(req.MsgID, o, r, c.uiWorkDir(sid))
	} else if !os.IsNotExist(err) {
		return uiFailure(req.MsgID, domain, argv, err, "error")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return uiFailure(req.MsgID, domain, argv, err, "error")
	}
	pending, _ := json.Marshal(uiJournal{Hash: digest})
	f, err := os.OpenFile(file, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return uiFailure(req.MsgID, domain, argv, err, "error")
	}
	_, err = f.Write(pending)
	if err == nil {
		err = f.Sync()
	}
	cerr := f.Close()
	if err == nil {
		err = cerr
	}
	if err != nil {
		return uiFailure(req.MsgID, domain, argv, err, "error")
	}
	var response *proto.ToolResponse
	if ctx.Err() != nil {
		response = uiFailure(req.MsgID, domain, argv, ui.Err("timeout", ctx.Err().Error()), "error")
	} else {
		response = fn()
	}
	normalizeUIResponse(response, domain, argv)
	b, err := json.Marshal(uiJournal{Hash: digest, Response: response})
	if err == nil {
		temp := file + ".tmp"
		err = os.WriteFile(temp, b, 0600)
		if err == nil {
			err = os.Rename(temp, file)
		}
	}
	if err != nil {
		response.Content = ui.AddWarning(response.Content, "journal_failed", "request result could not be journaled; retrying this request will not replay it: "+err.Error())
	}
	return response
}
