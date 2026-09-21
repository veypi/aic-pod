package browser

import (
	"context"
	"encoding/json"
	"fmt"
	tool "github.com/veypi/aic-pod/libs/hosts_tool"
	wire "github.com/veypi/aic-pod/protocol/hosts_tools"
	"math"
	"strings"
	"time"
)

func (p *page) controlAllowed(c tool.Caller) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.lease != nil && p.lease.connection != c.ConnectionID {
		return wire.Fail("control_busy", "Page is under manual control")
	}
	return nil
}
func (p *page) write(ctx context.Context, c tool.Caller, fn func() error) error {
	p.mu.Lock()
	lease := p.lease
	generation := p.controlEpoch
	p.mu.Unlock()
	if lease != nil {
		if lease.connection != c.ConnectionID {
			return wire.Fail("control_busy", "Page is under manual control")
		}
		// A navigation from the same viewer ends its input locally in browser.
		// No frontend acquire/release handshake is needed.
		lease.input.Close()
		p.mu.Lock()
		generation = p.controlEpoch
		p.mu.Unlock()
	}
	select {
	case p.gate <- struct{}{}:
		defer func() { <-p.gate }()
	case <-ctx.Done():
		return ctx.Err()
	}
	if err := c.Validate(ctx); err != nil {
		return err
	}
	p.mu.Lock()
	if p.closed || p.info.Uncertain {
		p.mu.Unlock()
		return wire.Fail("uncertain", "Page is closed or has an unresolved action; close the page before retrying")
	}
	if p.lease != nil || p.controlEpoch != generation {
		p.mu.Unlock()
		return wire.Fail("control_busy", "Page control changed while queued")
	}
	p.info.Revision++
	p.mu.Unlock()
	err := fn()
	if ctx.Err() != nil {
		p.mu.Lock()
		p.info.Uncertain = true
		p.mu.Unlock()
	}
	return err
}
func (s *Service) Navigate(ctx context.Context, c tool.Caller, a NavigateArgs) (Result, error) {
	p, err := s.get(c, a.PageID)
	if err != nil {
		return Result{}, err
	}
	err = p.write(ctx, c, func() error {
		var r struct {
			Error string `json:"errorText"`
		}
		if err := p.call(ctx, "Page.navigate", map[string]any{"url": a.URL}, &r); err != nil {
			return err
		}
		if r.Error != "" {
			return fmt.Errorf("navigation: %s", r.Error)
		}
		return nil
	})
	return Result{PageInfo: p.snapshot(), Effect: effect(err)}, err
}
func effect(err error) string {
	if err != nil {
		return "unknown"
	}
	return "applied"
}
func (s *Service) history(ctx context.Context, c tool.Caller, a PageArgs, delta int) (Result, error) {
	p, err := s.get(c, a.PageID)
	if err != nil {
		return Result{}, err
	}
	err = p.write(ctx, c, func() error {
		if delta == 0 {
			return p.call(ctx, "Page.reload", map[string]any{}, nil)
		}
		var h struct {
			Current int `json:"currentIndex"`
			Entries []struct {
				ID int `json:"id"`
			} `json:"entries"`
		}
		if e := p.call(ctx, "Page.getNavigationHistory", map[string]any{}, &h); e != nil {
			return e
		}
		i := h.Current + delta
		if i < 0 || i >= len(h.Entries) {
			return wire.Fail("not_found", "No history entry")
		}
		return p.call(ctx, "Page.navigateToHistoryEntry", map[string]any{"entryId": h.Entries[i].ID}, nil)
	})
	return Result{PageInfo: p.snapshot(), Effect: effect(err)}, err
}
func (p *page) evaluate(ctx context.Context, expression string) (json.RawMessage, error) {
	var r struct {
		Result struct {
			Value json.RawMessage `json:"value"`
		} `json:"result"`
		Exception json.RawMessage `json:"exceptionDetails"`
	}
	err := p.call(ctx, "Runtime.evaluate", map[string]any{"expression": expression, "returnByValue": true, "awaitPromise": true}, &r)
	if err != nil {
		return nil, err
	}
	if len(r.Exception) > 0 {
		return nil, wire.Fail("evaluation_failed", bounded(string(r.Exception), 2048))
	}
	if len(r.Result.Value) == 0 {
		return json.RawMessage("null"), nil
	}
	return r.Result.Value, nil
}
func (s *Service) Evaluate(ctx context.Context, c tool.Caller, a EvaluateArgs) (Result, error) {
	p, err := s.get(c, a.PageID)
	if err != nil {
		return Result{}, err
	}
	var v json.RawMessage
	err = p.write(ctx, c, func() error { var e error; v, e = p.evaluate(ctx, a.Code); return e })
	return Result{PageInfo: p.snapshot(), Effect: effect(err), Data: v}, err
}
func (s *Service) Dialog(ctx context.Context, c tool.Caller, a DialogArgs) (Result, error) {
	p, err := s.get(c, a.PageID)
	if err != nil {
		return Result{}, err
	}
	if err = p.controlAllowed(c); err != nil {
		return Result{}, err
	}
	p.mu.Lock()
	valid := p.info.Dialog != nil && p.info.Dialog.ID == a.ID
	p.mu.Unlock()
	if !valid {
		return Result{}, wire.Fail("stale_dialog", "Dialog changed")
	}
	err = p.call(ctx, "Page.handleJavaScriptDialog", map[string]any{"accept": a.Accept, "promptText": a.Text}, nil)
	return Result{PageInfo: p.snapshot(), Effect: effect(err)}, err
}
func (s *Service) Events(ctx context.Context, c tool.Caller, a EventsArgs) (map[string]any, error) {
	p, err := s.get(c, a.PageID)
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	out := []Event{}
	gap := len(p.events) > 0 && a.Cursor > 0 && a.Cursor < p.events[0].Cursor-1
	for _, e := range p.events {
		if e.Cursor > a.Cursor && (a.Kind == "" || strings.Contains(","+a.Kind+",", ","+e.Kind+",")) {
			out = append(out, e)
		}
	}
	return map[string]any{"events": out, "cursor": p.cursor, "gap": gap}, nil
}

type axValue struct {
	Value any `json:"value"`
}
type axNode struct {
	ID      string  `json:"nodeId"`
	Backend int     `json:"backendDOMNodeId"`
	Ignored bool    `json:"ignored"`
	Role    axValue `json:"role"`
	Name    axValue `json:"name"`
	Value   axValue `json:"value"`
}

func (p *page) ax(ctx context.Context) ([]axNode, error) {
	var tree struct {
		Nodes []axNode `json:"nodes"`
	}
	err := p.call(ctx, "Accessibility.getFullAXTree", map[string]any{}, &tree)
	return tree.Nodes, err
}
func (s *Service) Observe(ctx context.Context, c tool.Caller, a ObserveArgs) (Observation, error) {
	p, err := s.get(c, a.PageID)
	if err != nil {
		return Observation{}, err
	}
	return s.observe(ctx, c, p, a)
}
func (s *Service) observe(ctx context.Context, c tool.Caller, p *page, a ObserveArgs) (Observation, error) {
	before := p.snapshot()
	nodes, err := p.ax(ctx)
	if err != nil {
		return Observation{}, err
	}
	limit := a.Limit
	if limit == 0 {
		limit = 200
	}
	out := Observation{PageInfo: before, ID: wire.NewID("o_"), Elements: []Element{}}
	refs := map[string]reference{}
	for _, n := range nodes {
		role, name := fmt.Sprint(n.Role.Value), strings.TrimSpace(fmt.Sprint(n.Name.Value))
		if n.Ignored || n.Backend == 0 || role == "none" || role == "generic" {
			continue
		}
		if a.Query != "" && !strings.Contains(strings.ToLower(role+" "+name), strings.ToLower(a.Query)) {
			continue
		}
		if len(out.Elements) >= limit {
			out.Truncated = true
			break
		}
		id := wire.NewID("ref_")
		refs[id] = reference{backend: n.Backend, role: role, name: name, document: before.Document, subject: c.Subject, created: time.Now()}
		out.Elements = append(out.Elements, Element{Ref: id, Role: role, Name: bounded(name, 2048), Value: n.Value.Value})
	}
	if a.Image {
		var r struct {
			Data string `json:"data"`
		}
		if err = p.call(ctx, "Page.captureScreenshot", map[string]any{"format": "jpeg", "quality": 65, "captureBeyondViewport": false}, &r); err != nil {
			return Observation{}, err
		}
		if len(r.Data) > 400<<10 {
			return Observation{}, wire.Fail("output_limit", "Image exceeds call limit; use page.frames")
		}
		out.Image = r.Data
		out.MediaType = "image/jpeg"
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.info.Document != before.Document || p.info.Revision != before.Revision {
		return Observation{}, wire.Fail("observation_changed", "Page changed during observation")
	}
	for id, r := range p.refs {
		if time.Since(r.created) > 2*time.Minute {
			delete(p.refs, id)
		}
	}
	if len(p.refs)+len(refs) > 4096 {
		return Observation{}, wire.Fail("overloaded", "Observation ref budget reached; wait for old refs to expire")
	}
	for id, r := range refs {
		p.refs[id] = r
	}
	return out, nil
}
func (p *page) resolve(ctx context.Context, c tool.Caller, l Locator) (string, error) {
	if err := l.Validate(); err != nil {
		return "", err
	}
	backend := 0
	if l.Ref != "" {
		p.mu.Lock()
		r, ok := p.refs[l.Ref]
		doc := p.info.Document
		p.mu.Unlock()
		if !ok || r.subject != c.Subject || r.document != doc || time.Since(r.created) > 2*time.Minute {
			return "", wire.Fail("stale_ref", "Observation ref expired")
		}
		nodes, err := p.ax(ctx)
		if err != nil {
			return "", err
		}
		for _, n := range nodes {
			if n.Backend == r.backend && !n.Ignored && fmt.Sprint(n.Role.Value) == r.role && strings.TrimSpace(fmt.Sprint(n.Name.Value)) == r.name {
				backend = n.Backend
				break
			}
		}
		if backend == 0 {
			return "", wire.Fail("stale_ref", "Observed node or its meaning changed")
		}
	} else if l.CSS != "" {
		var root struct {
			Root struct {
				ID int `json:"nodeId"`
			} `json:"root"`
		}
		if err := p.call(ctx, "DOM.getDocument", map[string]any{}, &root); err != nil {
			return "", err
		}
		var found struct {
			IDs []int `json:"nodeIds"`
		}
		if err := p.call(ctx, "DOM.querySelectorAll", map[string]any{"nodeId": root.Root.ID, "selector": l.CSS}, &found); err != nil {
			return "", err
		}
		if len(found.IDs) != 1 {
			return "", wire.Fail("locator_match", fmt.Sprintf("Expected one element, matched %d", len(found.IDs)))
		}
		var resolved struct {
			Object struct {
				ID string `json:"objectId"`
			} `json:"object"`
		}
		err := p.call(ctx, "DOM.resolveNode", map[string]any{"nodeId": found.IDs[0]}, &resolved)
		return resolved.Object.ID, err
	} else {
		nodes, err := p.ax(ctx)
		if err != nil {
			return "", err
		}
		matches := 0
		for _, n := range nodes {
			if n.Ignored || n.Backend == 0 {
				continue
			}
			role, name := fmt.Sprint(n.Role.Value), strings.TrimSpace(fmt.Sprint(n.Name.Value))
			if l.Role != "" && role == l.Role && (l.Name == "" || name == l.Name) || l.Label != "" && name == l.Label && role != "StaticText" && role != "InlineTextBox" {
				backend = n.Backend
				matches++
			}
		}
		if matches != 1 {
			return "", wire.Fail("locator_match", fmt.Sprintf("Expected one element, matched %d", matches))
		}
	}
	var resolved struct {
		Object struct {
			ID string `json:"objectId"`
		} `json:"object"`
	}
	err := p.call(ctx, "DOM.resolveNode", map[string]any{"backendNodeId": backend}, &resolved)
	return resolved.Object.ID, err
}
func (p *page) node(ctx context.Context, object, fn string, args ...any) (json.RawMessage, error) {
	argv := []map[string]any{}
	for _, a := range args {
		argv = append(argv, map[string]any{"value": a})
	}
	var result struct {
		Result struct {
			Value json.RawMessage `json:"value"`
		} `json:"result"`
		Exception json.RawMessage `json:"exceptionDetails"`
	}
	err := p.call(ctx, "Runtime.callFunctionOn", map[string]any{"objectId": object, "functionDeclaration": fn, "arguments": argv, "returnByValue": true}, &result)
	if err != nil {
		return nil, err
	}
	if len(result.Exception) > 0 {
		return nil, wire.Fail("element_state", bounded(string(result.Exception), 1024))
	}
	return result.Result.Value, nil
}
func (p *page) release(object string) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = p.call(ctx, "Runtime.releaseObject", map[string]any{"objectId": object}, nil)
}
func (s *Service) action(name string) func(context.Context, tool.Caller, ActionArgs) (Result, error) {
	return func(ctx context.Context, c tool.Caller, a ActionArgs) (Result, error) {
		p, err := s.get(c, a.PageID)
		if err != nil {
			return Result{}, err
		}
		if name == "drag" && (math.IsNaN(a.X) || math.IsNaN(a.Y) || a.X < 0 || a.Y < 0 || a.X >= float64(p.info.Width) || a.Y >= float64(p.info.Height)) {
			return Result{}, errArg("Drag destination outside viewport")
		}
		err = p.write(ctx, c, func() error {
			object, err := p.resolve(ctx, c, a.Locator)
			if err != nil {
				return err
			}
			defer p.release(object)
			raw, err := p.node(ctx, object, `function(){ if(!this.isConnected)throw Error('detached');this.scrollIntoView({block:'center',inline:'center'});const r=this.getBoundingClientRect(),s=getComputedStyle(this);if(!r.width||!r.height||s.visibility==='hidden'||s.display==='none'||this.disabled)throw Error('not actionable');return {x:r.x+r.width/2,y:r.y+r.height/2}; }`)
			if err != nil {
				return err
			}
			var point struct{ X, Y float64 }
			if err = json.Unmarshal(raw, &point); err != nil {
				return err
			}
			mouse := func(kind, button string, buttons, count int, x, y float64) error {
				return p.call(ctx, "Input.dispatchMouseEvent", map[string]any{"type": kind, "x": x, "y": y, "button": button, "buttons": buttons, "clickCount": count}, nil)
			}
			switch name {
			case "click", "hover", "drag":
				raw, err = p.node(ctx, object, `function(){const r=this.getBoundingClientRect();const hit=document.elementFromPoint(r.x+r.width/2,r.y+r.height/2);return !!hit&&(hit===this||this.contains(hit));}`)
				if err != nil || string(raw) != "true" {
					return wire.Fail("element_state", "Element is obscured")
				}
				if name == "hover" {
					return mouse("mouseMoved", "none", 0, 0, point.X, point.Y)
				}
				released := false
				defer func() {
					if released {
						return
					}
					reset, cancel := context.WithTimeout(context.Background(), time.Second)
					defer cancel()
					_ = p.call(reset, "Input.dispatchMouseEvent", map[string]any{"type": "mouseReleased", "button": "left", "buttons": 0, "x": point.X, "y": point.Y, "clickCount": 1}, nil)
				}()
				if err = mouse("mousePressed", "left", 1, 1, point.X, point.Y); err != nil {
					return err
				}
				x, y := point.X, point.Y
				if name == "drag" {
					x, y = a.X, a.Y
					err = mouse("mouseMoved", "left", 1, 0, x, y)
				}
				releaseErr := mouse("mouseReleased", "left", 0, 1, x, y)
				released = releaseErr == nil
				if err != nil {
					return err
				}
				return releaseErr
			case "fill":
				_, err = p.node(ctx, object, `function(text){if(this.disabled||this.readOnly)throw Error('not editable');this.focus();if(this.isContentEditable){this.textContent=text;}else{const proto=this instanceof HTMLTextAreaElement?HTMLTextAreaElement.prototype:HTMLInputElement.prototype;const set=Object.getOwnPropertyDescriptor(proto,'value')?.set;if(!set)throw Error('not editable');set.call(this,text);}this.dispatchEvent(new Event('input',{bubbles:true}));this.dispatchEvent(new Event('change',{bubbles:true}));if((this.isContentEditable?this.textContent:this.value)!==text)throw Error('value not retained');return true;}`, a.Text)
				return err
			case "type", "press":
				if _, err = p.node(ctx, object, `function(){this.focus();if(document.activeElement!==this)throw Error('focus failed');return true;}`); err != nil {
					return err
				}
				if name == "type" {
					return p.call(ctx, "Input.insertText", map[string]any{"text": a.Text}, nil)
				}
				return p.key(ctx, a.Key)
			case "scroll":
				return p.call(ctx, "Input.dispatchMouseEvent", map[string]any{"type": "mouseWheel", "x": point.X, "y": point.Y, "deltaX": a.X, "deltaY": a.Y}, nil)
			case "set":
				_, err = p.node(ctx, object, `function(value){if(typeof value==='boolean'&&'checked' in this){if(this.checked!==value)this.click();if(this.checked!==value)throw Error('checked value not retained');}else if(this instanceof HTMLSelectElement){this.value=String(value);this.dispatchEvent(new Event('input',{bubbles:true}));this.dispatchEvent(new Event('change',{bubbles:true}));if(this.value!==String(value))throw Error('option not found');}else if(this instanceof HTMLInputElement&&this.type==='range'&&typeof value==='number'){this.value=String(value);this.dispatchEvent(new Event('input',{bubbles:true}));this.dispatchEvent(new Event('change',{bubbles:true}));if(Number(this.value)!==value)throw Error('range value not retained');}else throw Error('unsupported typed value');return true;}`, a.Value)
				return err
			}
			return wire.Fail("unsupported", "Unknown action")
		})
		r := Result{PageInfo: p.snapshot(), Effect: effect(err)}
		if err == nil && (a.After == "observation" || a.After == "image") {
			o, e := s.observe(ctx, c, p, ObserveArgs{PageID: a.PageID, Image: a.After == "image"})
			if e != nil {
				r.Warnings = []string{e.Error()}
			} else {
				r.Data = o
			}
		}
		return r, err
	}
}
func (p *page) key(ctx context.Context, key string) error {
	code := map[string]int{"Enter": 13, "Tab": 9, "Escape": 27, "Backspace": 8, "Delete": 46, "ArrowLeft": 37, "ArrowUp": 38, "ArrowRight": 39, "ArrowDown": 40, "Home": 36, "End": 35, "PageUp": 33, "PageDown": 34, "Space": 32}[key]
	if code == 0 {
		return errArg("Unsupported key %q", key)
	}
	if err := p.call(ctx, "Input.dispatchKeyEvent", map[string]any{"type": "keyDown", "key": key, "windowsVirtualKeyCode": code}, nil); err != nil {
		return err
	}
	return p.call(ctx, "Input.dispatchKeyEvent", map[string]any{"type": "keyUp", "key": key, "windowsVirtualKeyCode": code}, nil)
}
func (s *Service) Wait(ctx context.Context, c tool.Caller, a WaitArgs) (PageInfo, error) {
	p, err := s.get(c, a.PageID)
	if err != nil {
		return PageInfo{}, err
	}
	timeout := 5 * time.Second
	if a.TimeoutMS > 0 {
		timeout = time.Duration(a.TimeoutMS) * time.Millisecond
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	initial := p.snapshot().Document
	for {
		if err = c.Validate(ctx); err != nil {
			return PageInfo{}, err
		}
		match := false
		switch {
		case a.URL != "":
			match = p.snapshot().URL == a.URL
		case a.Text != "":
			v, e := p.evaluate(ctx, "Boolean(document.body?.innerText.includes("+jsonString(a.Text)+"))")
			err = e
			match = string(v) == "true"
		case a.Load:
			v, e := p.evaluate(ctx, "document.readyState === 'complete'")
			err = e
			match = string(v) == "true"
		case a.Locator != nil:
			object, e := p.resolve(ctx, c, *a.Locator)
			if e != nil {
				if f, ok := e.(*wire.Fault); ok && f.Code == "locator_match" {
					match = a.State == "hidden"
					err = nil
				} else {
					err = e
				}
			} else {
				v, e := p.node(ctx, object, `function(){const r=this.getBoundingClientRect();return {visible:!!(r.width&&r.height)&&getComputedStyle(this).visibility!=='hidden',enabled:!this.disabled};}`)
				p.release(object)
				err = e
				var st struct{ Visible, Enabled bool }
				_ = json.Unmarshal(v, &st)
				match = st.Visible
				if a.State == "hidden" {
					match = !st.Visible
				}
				if a.State == "enabled" {
					match = st.Visible && st.Enabled
				}
			}
		}
		if err != nil {
			return PageInfo{}, err
		}
		if match {
			return p.snapshot(), nil
		}
		if a.Locator != nil && a.Locator.Ref != "" && p.snapshot().Document != initial {
			return PageInfo{}, wire.Fail("stale_ref", "Document changed during wait")
		}
		select {
		case <-ctx.Done():
			return PageInfo{}, ctx.Err()
		case <-ticker.C:
		}
	}
}
