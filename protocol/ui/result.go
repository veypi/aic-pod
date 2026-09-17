package ui

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

type Result struct {
	Protocol    string              `json:"protocol"`
	Domain      string              `json:"domain"`
	Source      string              `json:"source,omitempty"`
	Op          string              `json:"op"`
	State       string              `json:"state"`
	Target      map[string]any      `json:"target,omitempty"`
	Action      map[string]any      `json:"action,omitempty"`
	Observation map[string]any      `json:"observation,omitempty"`
	Data        any                 `json:"data,omitempty"`
	Artifacts   []map[string]any    `json:"artifacts,omitempty"`
	Warnings    []map[string]string `json:"warnings,omitempty"`
	Error       *Error              `json:"error,omitempty"`
	Images      map[string]string   `json:"-"`
}

func NewResult(o *Operation) *Result {
	return &Result{Protocol: Schema.Protocol, Domain: o.Domain, Op: o.Op, State: "completed"}
}
func (r *Result) Fail(err error, performed any) {
	r.State = "error"
	var e *Error
	if !errors.As(err, &e) {
		e = Err("execution_failed", err.Error())
	}
	r.Error = e
	if performed != nil {
		r.Action = map[string]any{"performed": performed}
	}
}
func (r *Result) Warn(code, message string) {
	r.Warnings = append(r.Warnings, map[string]string{"code": code, "message": message})
}
func (r *Result) Render(format string) string {
	if format == "json" {
		b, _ := json.Marshal(r)
		return string(b)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "[ui/1] %s.%s state=%s\n", r.Domain, r.Op, r.State)
	block := func(k string, v any) {
		if v == nil {
			return
		}
		j, e := json.Marshal(v)
		if e == nil {
			fmt.Fprintf(&b, "\n[%s]\n%s\n", k, j)
		}
	}
	block("target", r.Target)
	block("action", r.Action)
	if r.Observation != nil {
		meta := map[string]any{}
		for k, v := range r.Observation {
			if k != "text" && k != "elements" {
				meta[k] = v
			}
		}
		block("observation", meta)
		if t, ok := r.Observation["text"].(string); ok {
			b.WriteString(t)
			b.WriteByte('\n')
		}
	}
	block("data", r.Data)
	if len(r.Artifacts) > 0 {
		block("artifacts", r.Artifacts)
	}
	if len(r.Warnings) > 0 {
		block("warnings", r.Warnings)
	}
	if r.Error != nil {
		block("error", r.Error)
	}
	return b.String()
}

// AddWarning/WithSource preserve either supported renderer without parsing observation text.
func AddWarning(content, code, message string) string {
	var r Result
	if json.Unmarshal([]byte(content), &r) == nil && r.Protocol == "ui/1" {
		r.Warn(code, message)
		return r.Render("json")
	}
	b, _ := json.Marshal([]map[string]string{{"code": code, "message": message}})
	return content + "\n[warnings]\n" + string(b) + "\n"
}
func WithSource(content, source string) string {
	if strings.HasPrefix(content, "[ui/1]") {
		b, _ := json.Marshal(source)
		i := strings.IndexByte(content, '\n')
		return content[:i+1] + "\n[source]\n" + string(b) + "\n" + content[i+1:]
	}
	var r map[string]any
	if json.Unmarshal([]byte(content), &r) == nil && r["protocol"] == "ui/1" {
		r["source"] = source
		b, _ := json.Marshal(r)
		return string(b)
	}
	return content
}
