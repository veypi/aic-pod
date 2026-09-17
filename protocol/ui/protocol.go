// Package ui defines the shared browser/cua ui/1 command contract.
package ui

import (
	"embed"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

//go:embed schema.json
var files embed.FS

type Command struct {
	Domains      []string `json:"domains"`
	Level        int      `json:"level"`
	Positions    []string `json:"positions"`
	Required     []string `json:"required"`
	Flags        []string `json:"flags"`
	Locator      string   `json:"locator"`
	Mutates      bool     `json:"mutates"`
	ObserveAfter bool     `json:"observe_after"`
	Description  string   `json:"description"`
	Overrides    map[string]struct {
		Positions []string `json:"positions"`
		Required  []string `json:"required"`
		Flags     []string `json:"flags"`
	} `json:"domain_overrides,omitempty"`
}
type Contract struct {
	Protocol string             `json:"protocol"`
	Flags    map[string]string  `json:"flags"`
	Common   []string           `json:"common_flags"`
	Locator  []string           `json:"locator_flags"`
	Commands map[string]Command `json:"commands"`
}

var Schema = func() Contract {
	b, _ := files.ReadFile("schema.json")
	var s Contract
	if err := json.Unmarshal(b, &s); err != nil {
		panic(err)
	}
	return s
}()

type Options struct {
	TimeoutMS int64  `json:"timeout_ms"`
	After     string `json:"after"`
	Format    string `json:"format"`
	Delivery  string `json:"delivery"`
}
type Operation struct {
	Domain  string         `json:"domain"`
	Op      string         `json:"op"`
	Target  string         `json:"target,omitempty"`
	Locator map[string]any `json:"locator,omitempty"`
	Args    map[string]any `json:"args"`
	Options Options        `json:"options"`
	Spec    Command        `json:"-"`
}
type Error struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
	Recovery  string `json:"recovery,omitempty"`
}

func (e *Error) Error() string        { return e.Message }
func Err(code, message string) *Error { return &Error{Code: code, Message: message} }
func invalid(s string) *Error         { return Err("invalid_argument", s) }
func Has(a []string, s string) bool {
	for _, v := range a {
		if v == s {
			return true
		}
	}
	return false
}
func Spec(domain, op string) (Command, bool) {
	c, ok := Schema.Commands[op]
	if !ok || !Has(c.Domains, domain) {
		return Command{}, false
	}
	if o, ok := c.Overrides[domain]; ok {
		c.Positions = o.Positions
		c.Required = o.Required
		c.Flags = o.Flags
	}
	return c, true
}
func Commands(domain string) []string {
	var a []string
	for k := range Schema.Commands {
		if _, ok := Spec(domain, k); ok {
			a = append(a, k)
		}
	}
	sort.Strings(a)
	return a
}

var numberRE = regexp.MustCompile(`^[+-]?(?:\d+\.?\d*|\.\d+)(?:[eE][+-]?\d+)?$`)

var durationRE = regexp.MustCompile(`^(\d+(?:\.\d+)?)(ms|s|m)$`)

func duration(s string) (int64, error) {
	m := durationRE.FindStringSubmatch(s)
	if m == nil {
		return 0, invalid("duration must include ms, s or m")
	}
	n, _ := strconv.ParseFloat(m[1], 64)
	scale := map[string]float64{"ms": 1, "s": 1000, "m": 60000}[m[2]]
	v := n * scale
	if v < 1 || v > 300000 || math.Trunc(v) != v {
		return 0, invalid("duration must be 1ms..5m in whole milliseconds")
	}
	return int64(v), nil
}
func scalar(kind, s string) (any, error) {
	switch {
	case kind == "string":
		return s, nil
	case kind == "duration":
		return duration(s)
	case kind == "json":
		var v any
		if e := json.Unmarshal([]byte(s), &v); e != nil {
			return nil, invalid("invalid JSON value")
		}
		return v, nil
	case strings.HasPrefix(kind, "enum:"):
		if Has(strings.Split(strings.TrimPrefix(kind, "enum:"), ","), s) {
			return s, nil
		}
		return nil, invalid("expected one of " + strings.TrimPrefix(kind, "enum:"))
	default:
		if !numberRE.MatchString(s) {
			return nil, invalid("expected a finite number")
		}
		n, e := strconv.ParseFloat(s, 64)
		if e != nil || math.IsNaN(n) || math.IsInf(n, 0) {
			return nil, invalid("expected a finite number")
		}
		if kind == "integer" && (math.Trunc(n) != n || n < 1 || n > 2147483647) {
			return nil, invalid("expected a positive integer")
		}
		if kind == "positive" && n <= 0 {
			return nil, invalid("expected a positive number")
		}
		return n, nil
	}
}
func Parse(domain string, argv []string) (*Operation, error) {
	if domain != "browser" && domain != "cua" {
		return nil, invalid("unknown UI domain")
	}
	if len(argv) == 0 {
		return nil, invalid("command required; use help")
	}
	op := argv[0]
	start := 1
	if len(argv) > 1 {
		if _, ok := Spec(domain, op+"."+argv[1]); ok {
			op += "." + argv[1]
			start = 2
		}
	}
	spec, ok := Spec(domain, op)
	if !ok {
		return nil, Err("unsupported", "unknown "+domain+" command "+op+"; use help")
	}
	allowed := append(append([]string{}, Schema.Common...), spec.Flags...)
	if spec.Locator != "none" {
		allowed = append(allowed, Schema.Locator...)
	}
	if spec.ObserveAfter {
		allowed = append(allowed, "after")
	}
	if spec.Mutates {
		allowed = append(allowed, "delivery")
	}
	flags := map[string]any{}
	var pos []string
	literal := false
	for i := start; i < len(argv); i++ {
		a := argv[i]
		if !literal && a == "--" {
			literal = true
			continue
		}
		if !literal && strings.HasPrefix(a, "--") {
			k := strings.TrimPrefix(a, "--")
			if !Has(allowed, k) {
				return nil, invalid("unsupported flag --" + k + " for " + op)
			}
			if _, ok := flags[k]; ok {
				return nil, invalid("duplicate flag --" + k)
			}
			kind := Schema.Flags[k]
			if kind == "bool" {
				flags[k] = true
				continue
			}
			n := 1
			if kind == "point" {
				n = 2
			}
			if i+n >= len(argv) {
				return nil, invalid("missing value for --" + k)
			}
			if kind == "point" {
				point := []float64{}
				for j := 0; j < 2; j++ {
					i++
					v, e := scalar("number", argv[i])
					if e != nil {
						return nil, e
					}
					point = append(point, v.(float64))
				}
				flags[k] = point
			} else {
				i++
				v, e := scalar(kind, argv[i])
				if e != nil {
					return nil, invalid("--" + k + ": " + e.Error())
				}
				flags[k] = v
			}
			continue
		}
		pos = append(pos, a)
	}
	if len(pos) > len(spec.Positions) {
		return nil, invalid("too many positional arguments for " + op)
	}
	for i, p := range spec.Positions {
		key := strings.TrimSuffix(p, "?")
		if i >= len(pos) {
			if key == p {
				return nil, invalid("missing " + key)
			}
			continue
		}
		if _, exists := flags[key]; exists {
			return nil, invalid("duplicate " + key)
		}
		flags[key] = pos[i]
	}
	for _, k := range spec.Required {
		if _, ok := flags[k]; !ok {
			return nil, invalid("missing --" + k)
		}
	}
	out := &Operation{Domain: domain, Op: op, Args: map[string]any{}, Locator: map[string]any{}, Spec: spec, Options: Options{TimeoutMS: 10000, After: "none", Format: "text", Delivery: "background"}}
	if spec.ObserveAfter {
		out.Options.After = "snapshot"
	}
	if op == "open" || op == "navigate" {
		out.Options.TimeoutMS = 30000
	}
	if op == "run" {
		out.Options.TimeoutMS = 60000
	}
	for k, v := range flags {
		switch k {
		case "target":
			out.Target = v.(string)
		case "timeout":
			out.Options.TimeoutMS = v.(int64)
		case "after":
			out.Options.After = v.(string)
		case "format":
			out.Options.Format = v.(string)
		case "delivery":
			out.Options.Delivery = v.(string)
		default:
			if spec.Locator != "none" && Has(Schema.Locator, k) {
				out.Locator[strings.ReplaceAll(k, "-", "_")] = v
			} else {
				out.Args[strings.ReplaceAll(k, "-", "_")] = v
			}
		}
	}
	if e := validate(out); e != nil {
		return nil, e
	}
	return out, nil
}
func validate(o *Operation) error {
	if o.Op == "run" {
		_, code := o.Args["code"]
		_, file := o.Args["file"]
		if code == file || code && strings.TrimSpace(o.String("code")) == "" || file && strings.TrimSpace(o.String("file")) == "" {
			return invalid("run requires exactly one nonempty --code or --file")
		}
		if len(o.String("code")) > 512*1024 {
			return invalid("script exceeds 512 KiB")
		}
	}
	l := o.Locator
	n := 0
	for _, k := range []string{"ref", "role", "label", "css", "at"} {
		if _, ok := l[k]; ok {
			n++
		}
	}
	if n > 1 {
		return invalid("choose one locator: ref, role/name, label, css or at")
	}
	if _, ok := l["name"]; ok && l["role"] == nil {
		return invalid("--name requires --role")
	}
	if l["contains"] != nil && l["role"] == nil && l["label"] == nil {
		return invalid("--contains requires a semantic locator")
	}
	if o.Spec.Locator == "required" && n == 0 {
		return invalid("locator required")
	}
	if v, ok := l["ref"].(string); ok && !regexp.MustCompile(`^@s[a-zA-Z0-9_-]+:e[1-9][0-9]*$`).MatchString(v) {
		return invalid("ref must be a snapshot-bound @s…:eN")
	}
	if l["at"] != nil && l["snapshot"] == nil {
		return invalid("coordinates require --snapshot")
	}
	if l["snapshot"] != nil && l["at"] == nil {
		return invalid("--snapshot requires coordinates")
	}
	if o.Domain == "cua" && l["css"] != nil {
		return Err("unsupported", "CSS locators are browser-only")
	}
	if o.Op == "scroll" && o.Args["dx"] == nil && o.Args["dy"] == nil {
		return invalid("scroll requires --dx or --dy")
	}
	if o.Op == "drag" {
		refs := o.Args["from"] != nil && o.Args["to"] != nil
		points := o.Args["from_at"] != nil && o.Args["to_at"] != nil && o.Args["snapshot"] != nil
		if refs == points {
			return invalid("drag requires --from/--to refs OR --from-at/--to-at/--snapshot")
		}
		if refs && (o.Args["from_at"] != nil || o.Args["to_at"] != nil || o.Args["snapshot"] != nil) {
			return invalid("mixed drag locators")
		}
		if points && (o.Args["from"] != nil || o.Args["to"] != nil) {
			return invalid("mixed drag locators")
		}
	}
	if o.Op == "wait" {
		if len(o.Locator) > 0 && o.Args["state"] == nil && o.Args["value"] == nil {
			return invalid("only state/value wait conditions accept a locator")
		}
		if o.Domain == "cua" && o.String("state") == "hidden" {
			return Err("unsupported", "native accessibility cannot prove absence")
		}
		if o.Number("ms") > 300000 {
			return invalid("--ms must be at most 300000")
		}
		conditions := 0
		for _, k := range []string{"text", "value", "state", "url", "load", "ms"} {
			if _, ok := o.Args[k]; ok {
				conditions++
			}
		}
		if conditions != 1 {
			return invalid("wait requires exactly one condition")
		}
		if o.Args["state"] != nil && n == 0 {
			return invalid("--state requires a locator")
		}
		if o.Args["value"] != nil && n == 0 {
			return invalid("--value requires a locator")
		}
		if o.Domain == "cua" && (o.Args["url"] != nil || o.Args["load"] != nil) {
			return Err("unsupported", "URL/load conditions are browser-only")
		}
	}
	if o.Op == "get" && !Has([]string{"title", "url", "text", "value", "checked", "bounds", "enabled"}, o.String("field")) {
		return invalid("unknown get field")
	}
	if o.Op == "menu" {
		a, ok := o.Args["path"].([]any)
		if !ok || len(a) == 0 {
			return invalid("menu path must be a nonempty JSON string array")
		}
		for _, v := range a {
			if s, ok := v.(string); !ok || s == "" {
				return invalid("menu path entries must be nonempty strings")
			}
		}
	}
	if o.Op == "set" {
		v := o.Args["value"]
		if v == nil {
			return invalid("set value cannot be null")
		}
		switch a := v.(type) {
		case map[string]any:
			return invalid("set value must be a scalar or string array")
		case []any:
			for _, x := range a {
				if _, ok := x.(string); !ok {
					return invalid("set array must contain strings")
				}
			}
		}
	}
	return nil
}
func (o *Operation) String(k string) string { v, _ := o.Args[k].(string); return v }
func (o *Operation) Number(k string) float64 {
	switch v := o.Args[k].(type) {
	case float64:
		return v
	case int64:
		return float64(v)
	}
	return 0
}
func (o *Operation) Bool(k string) bool { v, _ := o.Args[k].(bool); return v }
func (o *Operation) Level() int {
	if o.Options.Delivery == "foreground" {
		return 3
	}
	return o.Spec.Level
}
func Required(domain string, argv []string) int {
	o, e := Parse(domain, argv)
	if e != nil {
		return 3
	}
	return o.Level()
}
func Help(domain, command string) string {
	if command != "" {
		c, ok := Spec(domain, strings.ReplaceAll(command, " ", "."))
		if !ok {
			return "unknown command"
		}
		b, _ := json.MarshalIndent(c, "", "  ")
		return string(b)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s — ui/1\n\n", domain)
	for _, k := range Commands(domain) {
		c, _ := Spec(domain, k)
		fmt.Fprintf(&b, "  %-18s %s\n", strings.ReplaceAll(k, ".", " "), c.Description)
	}
	b.WriteString("\nObserve before acting. Refs: @s…:eN. Common: --target ID --timeout 10s --format text|json.\nActions return a fresh snapshot by default (--after none|snapshot|screenshot).\nUse help <command> for its schema. Run/driver-browser are not currently provided.\n")
	return b.String()
}
func (o *Operation) Timeout() time.Duration {
	return time.Duration(o.Options.TimeoutMS) * time.Millisecond
}

// HelpData is the structured runtime help, shared in shape with the JS renderer.
// Help remains the readable command-discovery description used by exec commands.
func HelpData(domain, command string) (any, error) {
	if command != "" {
		c, ok := Spec(domain, strings.ReplaceAll(command, " ", "."))
		if !ok {
			return nil, Err("unsupported", "unknown command")
		}
		return c, nil
	}
	entries := []map[string]string{}
	for _, op := range Commands(domain) {
		c, _ := Spec(domain, op)
		entries = append(entries, map[string]string{"op": op, "description": c.Description})
	}
	return map[string]any{"protocol": Schema.Protocol, "commands": entries, "locator": "snapshot ref @s…:eN, role/name, label, CSS (browser), or --at X Y --snapshot S", "common": "--target ID --timeout 10s --format text|json", "after": "Actions return a fresh snapshot (--after none|snapshot|screenshot)."}, nil
}
