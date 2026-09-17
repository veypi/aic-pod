package host

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/veypi/aic-pod/libs/proto"
	"github.com/veypi/aic-pod/libs/vcore"
	"github.com/veypi/aic-pod/protocol/ui"
)

type nativeCall func(context.Context, string, map[string]any) (*mcpResult, error)
type nativeElement struct {
	public map[string]any
	token  string
	raw    map[string]any
}
type nativeSnapshot struct {
	id                                 string
	elements                           map[string]*nativeElement
	order                              []*nativeElement
	width, height, rawWidth, rawHeight int
	bounds                             map[string]any
}
type nativeTarget struct {
	id                string
	pid, window       int
	birth, app, title string
	bounds            map[string]any
	snapshot          *nativeSnapshot
}
type nativeSession struct {
	current, driverSession string
	targets                map[string]*nativeTarget
}
type nativeUI struct {
	gate     chan struct{}
	epoch    uint64
	sessions map[string]*nativeSession
	identity func(context.Context, int) (string, error)
}

func newNativeUI() *nativeUI {
	return &nativeUI{gate: make(chan struct{}, 1), sessions: map[string]*nativeSession{}, identity: nativeProcessIdentity}
}

var cuaUI = newNativeUI()

func uiID(prefix string) string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return prefix + hex.EncodeToString(b)
}
func num(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	case int64:
		return float64(n)
	case json.Number:
		f, _ := n.Float64()
		return f
	}
	return 0
}
func str(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}
func nativeMaps(v any) []map[string]any {
	var out []map[string]any
	if a, ok := v.([]any); ok {
		for _, x := range a {
			if m, ok := x.(map[string]any); ok {
				out = append(out, m)
			}
		}
	}
	return out
}
func nativeData(r *mcpResult) map[string]any {
	if r == nil {
		return map[string]any{}
	}
	if r.StructuredContent != nil {
		return r.StructuredContent
	}
	for _, c := range r.Content {
		if c.Type == "text" {
			var m map[string]any
			if json.Unmarshal([]byte(c.Text), &m) == nil && m != nil {
				return m
			}
		}
	}
	return map[string]any{}
}
func (t *nativeTarget) public() map[string]any {
	return map[string]any{"id": t.id, "kind": "window", "title": t.title, "app": t.app, "bounds": t.bounds}
}
func nativeKey(t *nativeTarget) string { return fmt.Sprintf("%d:%d:%s", t.pid, t.window, t.birth) }
func (e *nativeUI) invalidate(t *nativeTarget) {
	for _, s := range e.sessions {
		for _, x := range s.targets {
			if nativeKey(x) == nativeKey(t) {
				x.snapshot = nil
			}
		}
	}
}
func nativeRole(s string) string {
	if r, ok := map[string]string{"AXButton": "button", "AXTextField": "textbox", "AXTextArea": "textbox", "AXCheckBox": "checkbox", "AXRadioButton": "radio", "AXPopUpButton": "combobox", "AXComboBox": "combobox", "AXSlider": "slider", "AXLink": "link", "AXMenuItem": "menuitem", "AXStaticText": "text", "AXWindow": "window", "AXTabGroup": "tablist"}[s]; ok {
		return r
	}
	return strings.ToLower(strings.TrimPrefix(s, "AX"))
}
func nativeProcessIdentity(ctx context.Context, pid int) (string, error) {
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.CommandContext(ctx, "powershell", "-NoProfile", "-NonInteractive", "-Command", fmt.Sprintf("(Get-Process -Id %d -ErrorAction Stop).StartTime.ToUniversalTime().Ticks", pid))
	} else {
		cmd = exec.CommandContext(ctx, "ps", "-p", strconv.Itoa(pid), "-o", "lstart=")
	}
	b, err := cmd.Output()
	if err != nil {
		return "", err
	}
	s := strings.TrimSpace(string(b))
	if s == "" {
		return "", fmt.Errorf("process identity unavailable")
	}
	return s, nil
}
func (e *nativeUI) discover(ctx context.Context, s *nativeSession, call nativeCall) ([]*nativeTarget, error) {
	res, err := call(ctx, "list_windows", map[string]any{})
	if err != nil {
		return nil, err
	}
	data := nativeData(res)
	windows := nativeMaps(data["windows"])
	live := map[string]bool{}
	births := map[int]string{}
	out := []*nativeTarget{}
	for _, w := range windows {
		pid, wid := int(num(w["pid"])), int(num(w["window_id"]))
		if pid <= 0 || wid <= 0 {
			continue
		}
		birth, ok := births[pid]
		if !ok {
			birth, err = e.identity(ctx, pid)
			if err != nil {
				continue
			}
			births[pid] = birth
		}
		key := fmt.Sprintf("%d:%d:%s", pid, wid, birth)
		live[key] = true
		var t *nativeTarget
		for _, old := range s.targets {
			if nativeKey(old) == key {
				t = old
				break
			}
		}
		if t == nil {
			t = &nativeTarget{id: uiID("w"), pid: pid, window: wid, birth: birth}
			s.targets[t.id] = t
		}
		t.app = str(w["app_name"])
		t.title = str(w["title"])
		t.bounds, _ = w["bounds"].(map[string]any)
		out = append(out, t)
	}
	for id, t := range s.targets {
		if !live[nativeKey(t)] {
			delete(s.targets, id)
			if s.current == id {
				s.current = ""
			}
		}
	}
	return out, nil
}
func nativeArgs(t *nativeTarget, session string) map[string]any {
	return map[string]any{"pid": t.pid, "window_id": t.window, "session": session}
}
func (e *nativeUI) observe(ctx context.Context, s *nativeSession, t *nativeTarget, call nativeCall, o *ui.Operation, r *ui.Result, root string, wantImage bool) (map[string]any, error) {
	e.invalidate(t)
	args := nativeArgs(t, s.driverSession)
	args["include_screenshot"] = wantImage
	if o.Number("depth") > 0 {
		args["max_depth"] = o.Number("depth")
	}
	res, err := call(ctx, "get_window_state", args)
	if err != nil {
		return nil, err
	}
	data := nativeData(res)
	snap := &nativeSnapshot{id: uiID("s"), elements: map[string]*nativeElement{}, bounds: t.bounds}
	var public []map[string]any
	var lines []string
	for _, raw := range nativeMaps(data["elements"]) {
		role := nativeRole(str(raw["role"]))
		name := str(raw["label"])
		if name == "" {
			name = str(raw["name"])
		}
		v := map[string]any{"role": role, "name": name, "native_role": raw["role"]}
		for _, k := range []string{"value", "focused", "enabled", "checked"} {
			if x, ok := raw[k]; ok {
				v[k] = x
			}
		}
		token := str(raw["element_token"])
		element := &nativeElement{public: v, token: token, raw: raw}
		if token != "" {
			ref := fmt.Sprintf("@%s:e%d", snap.id, len(snap.order)+1)
			v["ref"] = ref
			snap.elements[ref] = element
		}
		snap.order = append(snap.order, element)
		encoded, _ := json.Marshal(v)
		if o.String("query") != "" && !nativeQueryMatch(v, o.String("query")) {
			continue
		}
		if o.Bool("interactive") && token == "" {
			continue
		}
		public = append(public, v)
		lines = append(lines, string(encoded))
	}
	if len(lines) == 0 {
		lines = append(lines, "(no matching accessible elements)")
	}
	if v := str(data["window_title"]); v != "" {
		t.title = v
	}
	if v, ok := data["window_bounds"].(map[string]any); ok {
		t.bounds = v
		snap.bounds = v
	}
	obs := map[string]any{"snapshot": snap.id, "text": strings.Join(lines, "\n"), "elements": public, "truncated": num(data["element_count"]) < num(data["total_element_count"]), "observed_at": time.Now().UTC().Format(time.RFC3339Nano)}
	if data["degraded"] == true {
		r.Warn("accessibility_degraded", str(data["degraded_reason"]))
	}
	if wantImage {
		var raw []byte
		mime := "image/png"
		for _, c := range res.Content {
			if c.Type == "image" {
				raw, err = base64.StdEncoding.DecodeString(c.Data)
				if c.MimeType != "" {
					mime = c.MimeType
				}
				break
			}
		}
		if err != nil {
			return nil, err
		}
		if len(raw) == 0 {
			return nil, ui.Err("observation_failed", "driver did not provide a screenshot")
		}
		original, _, err := image.DecodeConfig(bytes.NewReader(raw))
		if err != nil {
			return nil, err
		}
		encoded, note, err := vcore.EncodeImageData(raw, mime)
		if err != nil {
			return nil, err
		}
		parts := strings.SplitN(encoded, ",", 2)
		delivered, err := base64.StdEncoding.DecodeString(parts[1])
		if err != nil {
			return nil, err
		}
		config, _, err := image.DecodeConfig(bytes.NewReader(delivered))
		if err != nil {
			return nil, err
		}
		ext := ".png"
		if strings.HasPrefix(encoded, "data:image/jpeg;") {
			ext = ".jpg"
			mime = "image/jpeg"
		}
		dir := filepath.Join(root, ".ui")
		if err = os.MkdirAll(dir, 0700); err != nil {
			return nil, err
		}
		file := filepath.Join(dir, snap.id+ext)
		if err = os.WriteFile(file, delivered, 0600); err != nil {
			return nil, err
		}
		r.Images = map[string]string{"image_data": encoded}
		if note != "" {
			r.Images["image_compressed"] = note
		}
		obs["image"] = map[string]any{"width": config.Width, "height": config.Height, "mime": mime, "path": file, "coordinate_space": "image_pixels", "full_page": false}
		r.Artifacts = append(r.Artifacts, map[string]any{"kind": "image", "path": file, "bytes": len(delivered), "mime": mime})
		snap.width = config.Width
		snap.height = config.Height
		snap.rawWidth = original.Width
		snap.rawHeight = original.Height
	}
	t.snapshot = snap
	return obs, nil
}
func (e *nativeUI) resolve(ctx context.Context, s *nativeSession, t *nativeTarget, call nativeCall, locator map[string]any, o *ui.Operation, r *ui.Result, root string) (map[string]any, *nativeElement, error) {
	args := nativeArgs(t, s.driverSession)
	if ref := str(locator["ref"]); ref != "" {
		if t.snapshot == nil || t.snapshot.elements[ref] == nil {
			return nil, nil, ui.Err("stale_ref", "ref does not belong to the current session/target snapshot")
		}
		el := t.snapshot.elements[ref]
		args["element_token"] = el.token
		return args, el, nil
	}
	if p, ok := locator["at"].([]float64); ok {
		snap := t.snapshot
		if snap == nil || snap.id != str(locator["snapshot"]) || snap.width == 0 {
			return nil, nil, ui.Err("stale_ref", "coordinates require the current window image snapshot")
		}
		if p[0] < 0 || p[1] < 0 || p[0] >= float64(snap.width) || p[1] >= float64(snap.height) {
			return nil, nil, ui.Err("invalid_argument", "point is outside screenshot")
		}
		a, _ := json.Marshal(snap.bounds)
		b, _ := json.Marshal(t.bounds)
		if !bytes.Equal(a, b) {
			return nil, nil, ui.Err("stale_ref", "window geometry changed")
		}
		args["x"] = p[0] * float64(snap.rawWidth) / float64(snap.width)
		args["y"] = p[1] * float64(snap.rawHeight) / float64(snap.height)
		return args, nil, nil
	}
	if locator["role"] != nil || locator["label"] != nil {
		if _, err := e.observe(ctx, s, t, call, &ui.Operation{Args: map[string]any{}}, r, root, false); err != nil {
			return nil, nil, err
		}
		var matches []*nativeElement
		eq := func(a, b string) bool {
			if locator["contains"] == true {
				return strings.Contains(a, b)
			}
			return a == b
		}
		for _, el := range t.snapshot.order {
			if el.token == "" {
				continue
			}
			if locator["label"] != nil {
				if eq(str(el.public["name"]), str(locator["label"])) {
					matches = append(matches, el)
				}
			} else if str(el.public["role"]) == str(locator["role"]) && (locator["name"] == nil || eq(str(el.public["name"]), str(locator["name"]))) {
				matches = append(matches, el)
			}
		}
		if len(matches) == 0 {
			return nil, nil, ui.Err("not_found", "semantic locator did not match")
		}
		if len(matches) > 1 {
			return nil, nil, ui.Err("ambiguous_target", "semantic locator matched multiple elements")
		}
		args["element_token"] = matches[0].token
		return args, matches[0], nil
	}
	return args, nil, nil
}
func nativePress(s string) (string, []string, error) {
	parts := strings.Split(s, "+")
	key := parts[len(parts)-1]
	mods := []string{}
	seen := map[string]bool{}
	for _, m := range parts[:len(parts)-1] {
		if m == "ControlOrMeta" {
			m = "Control"
			if runtime.GOOS == "darwin" {
				m = "Meta"
			}
		}
		v := map[string]string{"Control": "ctrl", "Meta": "cmd", "Alt": "alt", "Shift": "shift"}[m]
		if v == "" || seen[v] {
			return "", nil, ui.Err("invalid_argument", "invalid key modifier")
		}
		seen[v] = true
		mods = append(mods, v)
	}
	if v, ok := map[string]string{"Enter": "return", "Tab": "tab", "Escape": "escape", "Backspace": "delete", "Delete": "forwarddelete", "ArrowLeft": "left", "ArrowRight": "right", "ArrowUp": "up", "ArrowDown": "down", "Home": "home", "End": "end", "PageUp": "pageup", "PageDown": "pagedown", "Space": "space"}[key]; ok {
		return v, mods, nil
	}
	if len(key) == 1 && strings.Contains("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789", key) {
		return strings.ToLower(key), mods, nil
	}
	if strings.HasPrefix(key, "F") {
		n, err := strconv.Atoi(key[1:])
		if err == nil && n >= 1 && n <= 12 {
			return strings.ToLower(key), mods, nil
		}
	}
	return "", nil, ui.Err("invalid_argument", "unsupported key")
}

func (e *nativeUI) execute(ctx context.Context, o *ui.Operation, sid, root string, epoch uint64, call nativeCall) *ui.Result {
	r := ui.NewResult(o)
	select {
	case e.gate <- struct{}{}:
		defer func() { <-e.gate }()
	case <-ctx.Done():
		r.Fail(ui.Err("timeout", ctx.Err().Error()), false)
		return r
	}
	if err := ctx.Err(); err != nil {
		r.Fail(ui.Err("timeout", err.Error()), false)
		return r
	}
	if e.epoch != epoch {
		e.epoch = epoch
		e.sessions = map[string]*nativeSession{}
	}
	s := e.sessions[sid]
	if s == nil {
		if len(e.sessions) >= 256 {
			r.Fail(ui.Err("resource_limit", "too many active UI sessions"), false)
			return r
		}
		s = &nativeSession{targets: map[string]*nativeTarget{}, driverSession: uiID("aicui-")}
		e.sessions[sid] = s
	}
	var t *nativeTarget
	started := false
	finishError := func(err error) *ui.Result {
		err = nativeUIError(err)
		performed := any(false)
		if started {
			performed = "unknown"
		}
		r.Fail(err, performed)
		if t != nil {
			r.Target = t.public()
			e.invalidate(t)
		}
		if cuaSessionEndedErr(err) {
			delete(e.sessions, sid)
			r.Error = ui.Err("session_expired", "driver session ended; the next command will restore the connection; discover and bind a new target")
		}
		return r
	}
	action := func(tool string, args map[string]any) (*mcpResult, error) {
		if err := ctx.Err(); err != nil {
			return nil, ui.Err("timeout", err.Error())
		}
		started = true
		res, err := call(ctx, tool, args)
		if t != nil {
			e.invalidate(t)
		}
		if err == nil {
			data := nativeData(res)
			if data["status"] == "refused" || data["status"] == "error" {
				return nil, ui.Err("delivery_failed", fmt.Sprint(data))
			}
			r.Action = map[string]any{"performed": true, "delivery": o.Options.Delivery}
			if data["effect"] == "unverifiable" {
				r.Warn("effect_unverified", "driver delivered input without confirming the application effect")
			}
			r.Data = data
		}
		return res, err
	}
	if o.Op == "help" {
		var err error
		r.Data, err = ui.HelpData("cua", o.String("command"))
		if err != nil {
			return finishError(err)
		}
		return r
	}
	if o.Op == "capabilities" {
		r.Data = map[string]any{"protocol": "ui/1", "backend": "cua-driver", "commands": ui.Commands("cua"), "locators": []string{"ref", "role/name", "label", "image_pixels"}, "delivery": []string{"background", "foreground"}, "fill": "native editable AX values; web AX content is rejected", "move": "agent cursor overlay only", "scroll": "line approximation (40 logical pixels per line)", "run": "Host JS worker with ui API; level 3, confined process, sequential calls, 256 steps, 60s default / 5m max; no Node or direct OS APIs.", "unsupported": []string{"CSS", "desktop input", "browser page commands", "ref-based wait", "ref-based drag", "ref-based move", "type without locator", "web AX fill/set", "wait hidden"}}
		return r
	}
	if o.Op == "doctor" {
		r.Data = map[string]any{"driver": findCuaDriver(), "runtime_epoch": epoch}
		return r
	}
	if o.Op == "apps" {
		res, err := call(ctx, "list_apps", map[string]any{})
		if err != nil {
			return finishError(err)
		}
		r.Data = nativeData(res)
		return r
	}
	if strings.HasPrefix(o.Op, "clipboard.") || strings.HasPrefix(o.Op, "cursor.") {
		args := map[string]any{"session": s.driverSession}
		tool := ""
		switch o.Op {
		case "clipboard.read":
			tool = "clipboard_read"
			args["include_text"] = true
		case "clipboard.write":
			tool = "clipboard_write"
			args["text"] = o.String("text")
		case "cursor.on", "cursor.off":
			tool = "set_agent_cursor_enabled"
			args["enabled"] = o.Op == "cursor.on"
		case "cursor.state":
			tool = "get_agent_cursor_state"
		}
		var res *mcpResult
		var err error
		if o.Op == "clipboard.write" {
			res, err = action(tool, args)
		} else {
			res, err = call(ctx, tool, args)
		}
		if err != nil {
			return finishError(err)
		}
		r.Data = nativeData(res)
		return r
	}
	var windows []*nativeTarget
	var err error
	if o.Op == "open" {
		res, er := action("launch_app", map[string]any{"name": o.String("app")})
		if er != nil {
			return finishError(er)
		}
		pid := int(num(nativeData(res)["pid"]))
		windows, err = e.discover(ctx, s, call)
		if err != nil {
			return finishError(err)
		}
		var found []*nativeTarget
		for _, w := range windows {
			if w.pid == pid {
				found = append(found, w)
			}
		}
		if len(found) == 1 {
			t = found[0]
			s.current = t.id
		} else {
			candidates := []map[string]any{}
			for _, w := range found {
				candidates = append(candidates, w.public())
			}
			r.Data = map[string]any{"targets": candidates, "selection_required": true}
			return r
		}
	} else {
		windows, err = e.discover(ctx, s, call)
		if err != nil {
			return finishError(err)
		}
	}
	if o.Op == "target.list" {
		out := []map[string]any{}
		for _, w := range windows {
			if o.String("app") != "" && !strings.EqualFold(o.String("app"), w.app) {
				continue
			}
			if o.Number("pid") > 0 && int(o.Number("pid")) != w.pid {
				continue
			}
			out = append(out, w.public())
		}
		r.Data = out
		return r
	}
	if t == nil {
		target := o.Target
		if target == "" {
			target = s.current
		}
		if o.Op == "target.use" {
			target = o.String("id")
		}
		if target == "" {
			return finishError(ui.Err("target_required", "select a target with target use or open"))
		}
		t = s.targets[target]
		if t == nil {
			return finishError(ui.Err("target_closed", "target is not available in this session"))
		}
		if o.Op == "target.use" {
			s.current = t.id
		}
	}
	r.Target = t.public()
	op := o.Op
	switch op {
	case "open", "target.use", "target.current":
	case "snapshot", "screenshot":
		if o.Bool("full") {
			return finishError(ui.Err("unsupported", "full-page screenshots are browser-only"))
		}
		r.Observation, err = e.observe(ctx, s, t, call, o, r, root, op == "screenshot" || o.Bool("image"))
	case "read", "get":
		if len(o.Locator) == 0 {
			if op == "get" {
				switch o.String("field") {
				case "title":
					r.Data = map[string]any{"title": t.title}
				case "bounds":
					r.Data = map[string]any{"bounds": t.bounds}
				case "text":
					r.Observation, err = e.observe(ctx, s, t, call, o, r, root, false)
				default:
					err = ui.Err("invalid_argument", "field requires an element locator")
				}
			} else {
				r.Observation, err = e.observe(ctx, s, t, call, o, r, root, false)
			}
			if err == nil && (op == "read" || o.String("field") == "text") {
				r.Data = map[string]any{"text": nativeText(t.snapshot)}
			}
		} else {
			var el *nativeElement
			_, el, err = e.resolve(ctx, s, t, call, o.Locator, o, r, root)
			if err == nil {
				if el == nil {
					err = ui.Err("unsupported", "read/get require an element")
				} else {
					field := o.String("field")
					if op == "read" {
						field = "text"
					}
					value := el.public[field]
					if field == "text" {
						value = el.public["value"]
						if value == nil {
							value = el.public["name"]
						}
					}
					if field == "bounds" {
						value = el.raw["frame"]
					}
					r.Data = map[string]any{field: value, "snapshot": t.snapshot.id, "source": "snapshot"}
				}
			}
		}
	case "wait":
		if o.Args["ms"] != nil {
			timer := time.NewTimer(time.Duration(o.Number("ms")) * time.Millisecond)
			select {
			case <-timer.C:
			case <-ctx.Done():
				err = ctx.Err()
			}
			timer.Stop()
		} else {
			for {
				if err = ctx.Err(); err != nil {
					break
				}
				obs, er := e.observe(ctx, s, t, call, &ui.Operation{Args: map[string]any{}}, r, root, false)
				if er != nil {
					err = er
					break
				}
				matched := false
				if o.Args["text"] != nil {
					for _, item := range t.snapshot.order {
						if strings.Contains(str(item.public["name"]), o.String("text")) || strings.Contains(str(item.public["value"]), o.String("text")) {
							matched = true
							break
						}
					}
				} else {
					if o.Locator["ref"] != nil {
						err = ui.Err("stale_ref", "native wait refreshes driver snapshots; use a semantic locator")
						break
					}
					_, el, er := e.resolve(ctx, s, t, call, o.Locator, o, r, root)
					if er != nil {
						if ue, ok := er.(*ui.Error); ok && ue.Code == "not_found" {
							matched = o.String("state") == "hidden"
						} else {
							err = er
							break
						}
					} else if o.Args["state"] != nil {
						switch o.String("state") {
						case "visible":
							matched = el != nil
						case "hidden":
							matched = el == nil
						case "enabled":
							matched = el != nil && el.public["enabled"] == true
						case "disabled":
							matched = el != nil && el.public["enabled"] == false
						}
					} else if el != nil {
						a, _ := json.Marshal(el.public["value"])
						b, _ := json.Marshal(o.Args["value"])
						matched = bytes.Equal(a, b)
					}
				}
				if matched {
					r.Observation = obs
					break
				}
				select {
				case <-ctx.Done():
					err = ctx.Err()
				case <-time.After(100 * time.Millisecond):
				}
				if err != nil {
					break
				}
			}
		}
		if err == nil {
			r.Data = map[string]any{"matched": true}
		}
	case "menu", "window.bounds", "activate":
		args := nativeArgs(t, s.driverSession)
		if op == "activate" {
			delete(args, "session")
		}
		tool := map[string]string{"menu": "invoke_menu", "window.bounds": "set_window_frame", "activate": "bring_to_front"}[op]
		if o.Options.Delivery == "foreground" && op != "activate" {
			err = ui.Err("unsupported", "this semantic operation has no foreground delivery option")
			break
		}
		if op == "menu" {
			args["path"] = o.Args["path"]
		}
		if op == "window.bounds" {
			for _, k := range []string{"x", "y", "width", "height"} {
				args[k] = o.Args[k]
			}
		}
		_, err = action(tool, args)
	case "drag":
		if o.Args["from"] != nil {
			err = ui.Err("unsupported", "native drag currently requires image coordinates")
			break
		}
		a, _, er := e.resolve(ctx, s, t, call, map[string]any{"at": o.Args["from_at"], "snapshot": o.Args["snapshot"]}, o, r, root)
		if er != nil {
			err = er
			break
		}
		b, _, er := e.resolve(ctx, s, t, call, map[string]any{"at": o.Args["to_at"], "snapshot": o.Args["snapshot"]}, o, r, root)
		if er != nil {
			err = er
			break
		}
		args := nativeArgs(t, s.driverSession)
		args["from_x"] = a["x"]
		args["from_y"] = a["y"]
		args["to_x"] = b["x"]
		args["to_y"] = b["y"]
		args["delivery_mode"] = o.Options.Delivery
		_, err = action("drag", args)
	default:
		var args map[string]any
		var el *nativeElement
		args, el, err = e.resolve(ctx, s, t, call, o.Locator, o, r, root)
		if err != nil {
			break
		}
		switch op {
		case "click":
			args["delivery_mode"] = o.Options.Delivery
			args["button"] = o.String("button")
			if args["button"] == "" {
				args["button"] = "left"
			}
			args["count"] = 1
			if o.String("count") == "2" {
				args["count"] = 2
				if el != nil {
					if args["button"] != "left" {
						err = ui.Err("unsupported", "native element double-click only supports the left button")
						break
					}
					delete(args, "button")
					delete(args, "count")
					_, err = action("double_click", args)
					break
				}
			}
			_, err = action("click", args)
		case "fill", "set":
			if el == nil {
				err = ui.Err("unsupported", "fill/set require an accessible element")
				break
			}
			if o.Options.Delivery != "background" {
				err = ui.Err("unsupported", "AX value setting has no foreground variant")
				break
			}
			value := o.Args["value"]
			if op == "fill" {
				if !ui.Has([]string{"textbox", "combobox"}, str(el.public["role"])) {
					err = ui.Err("unsupported", "element is not an editable native control")
					break
				}
				value = o.String("text")
			}
			if _, ok := value.([]any); ok {
				err = ui.Err("unsupported", "native multiselect values are not provided")
				break
			}
			if e.webElement(t, el) {
				err = ui.Err("unsupported", "web content requires browser fill, or cua type with image coordinates")
				break
			}
			args["value"] = str(value)
			_, err = action("set_value", args)
			if err == nil && op == "fill" {
				var obs map[string]any
				obs, err = e.observe(ctx, s, t, call, &ui.Operation{Args: map[string]any{}}, r, root, false)
				if err == nil {
					r.Observation = obs
					matches := []*nativeElement{}
					for _, item := range t.snapshot.order {
						if item.public["role"] == el.public["role"] && item.public["name"] == el.public["name"] {
							matches = append(matches, item)
						}
					}
					if len(matches) != 1 {
						err = ui.Err("verification_failed", "cannot uniquely verify filled native control")
					} else if str(matches[0].public["value"]) != str(value) {
						err = ui.Err("verification_failed", "native value differs from requested text")
					} else {
						r.Action["verified"] = true
					}
				}
			}
		case "type":
			if el == nil && args["x"] == nil {
				err = ui.Err("focus_required", "native type requires an observed element or image locator")
				break
			}
			args["text"] = o.String("text")
			args["delivery_mode"] = o.Options.Delivery
			_, err = action("type_text", args)
		case "press":
			if el == nil && t.snapshot == nil {
				err = ui.Err("focus_required", "observe the exact window before sending a key")
				break
			}
			var key string
			var mods []string
			key, mods, err = nativePress(o.String("key"))
			if err != nil {
				break
			}
			args["key"] = key
			args["modifiers"] = mods
			args["delivery_mode"] = o.Options.Delivery
			_, err = action("press_key", args)
		case "scroll":
			r.Warn("unit_approximation", "native driver scrolls lines; 40 logical pixels are mapped to one line")
			args["delivery_mode"] = o.Options.Delivery
			for _, axis := range []string{"dx", "dy"} {
				delta := o.Number(axis)
				if delta == 0 {
					continue
				}
				direction := "down"
				if axis == "dx" {
					direction = "right"
				}
				if delta < 0 {
					direction = "up"
					if axis == "dx" {
						direction = "left"
					}
				}
				remaining := int(math.Ceil(math.Abs(delta) / 40))
				if remaining > 500 {
					err = ui.Err("invalid_argument", "native scroll exceeds 500 lines")
					break
				}
				for remaining > 0 {
					n := remaining
					if n > 50 {
						n = 50
					}
					args["direction"] = direction
					args["amount"] = n
					_, err = action("scroll", args)
					if err != nil {
						break
					}
					remaining -= n
				}
				if err != nil {
					break
				}
			}
		case "move":
			if args["x"] == nil {
				err = ui.Err("unsupported", "native cursor overlay requires screenshot coordinates")
				break
			}
			if o.Options.Delivery != "background" {
				err = ui.Err("unsupported", "native move is an overlay operation")
				break
			}
			ma := map[string]any{"target": map[string]any{"kind": "window", "pid": t.pid, "window_id": t.window}, "session": s.driverSession, "x": args["x"], "y": args["y"]}
			_, err = action("move_cursor", ma)
			if err == nil {
				r.Action["delivery"] = "cursor_overlay"
				r.Warn("overlay_only", "moves the agent cursor; does not synthesize application hover")
			}
		default:
			err = ui.Err("unsupported", "native operation is not implemented")
		}
	}
	if err != nil {
		return finishError(err)
	}
	if o.Options.After != "none" {
		r.Observation, err = e.observe(ctx, s, t, call, &ui.Operation{Args: map[string]any{}}, r, root, o.Options.After == "screenshot")
		if err != nil {
			r.Warn("observation_failed", err.Error())
		}
	}
	r.Target = t.public()
	return r
}

func (c *Client) runCua(ctx context.Context, sid string, req *proto.ToolRequest, argv []string) *proto.ToolResponse {
	o, err := ui.Parse("cua", argv)
	if err != nil {
		return uiFailure(req.MsgID, "cua", argv, err, "error")
	}
	if req.GrantedLevel < o.Level() {
		return uiFailure(req.MsgID, "cua", argv, ui.Err("permission_denied", "operation level was not granted"), "rejected")
	}
	if sid == "" {
		return uiFailure(req.MsgID, "cua", argv, ui.Err("invalid_argument", "session_id required"), "error")
	}
	ctx, cancel := context.WithTimeout(ctx, o.Timeout())
	defer cancel()
	if cuaRt == nil {
		return uiFailure(req.MsgID, "cua", argv, ui.Err("unsupported", "cua-driver is unavailable"), "error")
	}
	// Initialize once before resolving any target. Existing handles are bound to this epoch.
	cuaRt.callMu.Lock()
	err = cuaRt.ensure(ctx)
	cuaRt.mu.Lock()
	epoch := cuaRt.generation
	cuaRt.mu.Unlock()
	cuaRt.callMu.Unlock()
	if err != nil {
		return uiFailure(req.MsgID, "cua", argv, err, "error")
	}
	call := func(ctx context.Context, name string, args map[string]any) (*mcpResult, error) {
		return cuaRt.callEpoch(ctx, epoch, name, args)
	}
	r := cuaUI.execute(ctx, o, sid, c.uiWorkDir(sid), epoch, call)
	return uiResponse(req.MsgID, o, r, c.uiWorkDir(sid))
}

// Web accessibility values are not independent confirmation of renderer changes.
func (e *nativeUI) webElement(t *nativeTarget, el *nativeElement) bool {
	if t.snapshot == nil {
		return true
	}
	byIndex := map[int]*nativeElement{}
	for _, x := range t.snapshot.order {
		byIndex[int(num(x.raw["element_index"]))] = x
	}
	seen := map[*nativeElement]bool{}
	for el != nil && !seen[el] {
		seen[el] = true
		if str(el.raw["role"]) == "AXWebArea" {
			return true
		}
		parent, ok := el.raw["parent_index"]
		if !ok {
			break
		}
		el = byIndex[int(num(parent))]
	}
	return false
}
func (e *nativeUI) endSession(ctx context.Context, sid string) error {
	select {
	case e.gate <- struct{}{}:
		defer func() { <-e.gate }()
	case <-ctx.Done():
		return ctx.Err()
	}
	s := e.sessions[sid]
	delete(e.sessions, sid)
	if s != nil && cuaRt != nil {
		_, err := cuaRt.callEpoch(ctx, e.epoch, "end_session", map[string]any{"session": s.driverSession})
		return err
	}
	return nil
}

func nativeUIError(err error) error {
	var typed *ui.Error
	if errors.As(err, &typed) {
		return err
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return ui.Err("timeout", err.Error())
	}
	message := strings.ToLower(err.Error())
	if strings.Contains(message, "stale") {
		return ui.Err("stale_ref", err.Error())
	}
	if strings.Contains(message, "window_id_not_found") {
		return ui.Err("target_closed", err.Error())
	}
	if strings.Contains(message, "window_owner_pid_mismatch") {
		return ui.Err("target_closed", err.Error())
	}
	if strings.Contains(message, "runtime changed") {
		return ui.Err("session_expired", err.Error())
	}
	return err
}

func nativeText(s *nativeSnapshot) string {
	if s == nil {
		return ""
	}
	lines := []string{}
	for _, el := range s.order {
		name, value := str(el.public["name"]), str(el.public["value"])
		if name != "" {
			lines = append(lines, name)
		}
		if value != "" && value != name {
			lines = append(lines, value)
		}
	}
	return strings.Join(lines, "\n")
}

func nativeQueryMatch(element map[string]any, query string) bool {
	query = strings.ToLower(query)
	for _, key := range []string{"name", "role", "value"} {
		if value, ok := element[key]; ok && strings.Contains(strings.ToLower(str(value)), query) {
			return true
		}
	}
	return false
}
