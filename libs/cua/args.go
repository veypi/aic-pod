package cua

// args.go 是 cua 指令的参数类型（vsh 指令面经 cliargs 解析填充；
// 不再有 hosts_tool 的 schema/审批元数据）。

import (
	"encoding/json"

	wire "github.com/veypi/aic-pod/protocol/hosts_tools"
)

type Empty struct{}
type WindowArgs struct {
	WindowID string `json:"window_id" required:"true"`
}
type ListArgs struct {
	App string `json:"app,omitempty"`
	PID int    `json:"pid,omitempty"`
}
type OpenArgs struct {
	App string `json:"app" required:"true"`
}
type ObserveArgs struct {
	WindowID string `json:"window_id" required:"true"`
	Image    bool   `json:"image,omitempty"`
	Query    string `json:"query,omitempty"`
	Depth    int    `json:"depth,omitempty"`
}
type Locator struct {
	Ref      string    `json:"ref,omitempty"`
	Role     string    `json:"role,omitempty"`
	Name     string    `json:"name,omitempty"`
	Label    string    `json:"label,omitempty"`
	At       []float64 `json:"at,omitempty"`
	Snapshot string    `json:"snapshot,omitempty"`
}

func (l Locator) Validate() error {
	n := 0
	for _, v := range []string{l.Ref, l.Role, l.Label} {
		if v != "" {
			n++
		}
	}
	if len(l.At) > 0 {
		n++
		if len(l.At) != 2 || l.Snapshot == "" {
			return wire.Fail("invalid_argument", "Coordinates require two values and snapshot")
		}
	}
	if n != 1 || l.Name != "" && l.Role == "" {
		return wire.Fail("invalid_argument", "Provide one locator: ref, role/name, label or snapshot coordinates")
	}
	return nil
}

type ActionArgs struct {
	WindowID string  `json:"window_id" required:"true"`
	Locator  Locator `json:"locator" required:"true"`
	Text     string  `json:"text,omitempty"`
	Key      string  `json:"key,omitempty"`
	Value    any     `json:"value,omitempty"`
	X        float64 `json:"x,omitempty"`
	Y        float64 `json:"y,omitempty"`
	Button   string  `json:"button,omitempty" enum:"left,right,middle"`
	Count    int     `json:"count,omitempty"`
	Delivery string  `json:"delivery,omitempty" enum:"background,foreground"`
	After    string  `json:"after,omitempty" enum:"none,observation,image"`
}

func (a *ActionArgs) Validate() error {
	if !wire.ValidID(a.WindowID) || a.Count < 0 || a.Count > 2 {
		return wire.Fail("invalid_argument", "Invalid window or click count")
	}
	return a.Locator.Validate()
}

type WaitArgs struct {
	WindowID string   `json:"window_id" required:"true"`
	Locator  *Locator `json:"locator,omitempty"`
	Text     *string  `json:"text,omitempty"`
	State    string   `json:"state,omitempty" enum:"visible,hidden,enabled,disabled"`
}
type DragArgs struct {
	WindowID string    `json:"window_id" required:"true"`
	Snapshot string    `json:"snapshot" required:"true"`
	From     []float64 `json:"from_at" required:"true"`
	To       []float64 `json:"to_at" required:"true"`
	Delivery string    `json:"delivery,omitempty" enum:"background,foreground"`
}
type MenuArgs struct {
	WindowID string   `json:"window_id" required:"true"`
	Path     []string `json:"path" required:"true"`
}
type BoundsArgs struct {
	WindowID string  `json:"window_id" required:"true"`
	X        float64 `json:"x" required:"true"`
	Y        float64 `json:"y" required:"true"`
	Width    float64 `json:"width" required:"true"`
	Height   float64 `json:"height" required:"true"`
}
type TextArgs struct {
	Text string `json:"text" required:"true"`
}
type CursorArgs struct {
	Enabled bool `json:"enabled" required:"true"`
}
type ImageArgs struct {
	Offset   int    `json:"offset,omitempty"`
	Limit    int    `json:"limit,omitempty"`
	WindowID string `json:"window_id" required:"true"`
	ImageID  string `json:"image_id" required:"true"`
}

func fields(v any) map[string]any {
	b, _ := json.Marshal(v)
	out := map[string]any{}
	_ = json.Unmarshal(b, &out)
	return out
}
