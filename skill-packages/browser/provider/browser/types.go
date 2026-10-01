// Package browser owns the dedicated Chrome, pages, observations, downloads and viewers.
//
// v6 P5：本包是 skill-packages/browser 的能力内核，由 svc provider（外部进程）
// 承载——pod 单用户语义：无 caller/subject 身份模型（权限 = 进程沙箱），页面与
// 下载全部归属本服务唯一主体。
package browser

import (
	"container/list"
	"encoding/json"
	"fmt"
	wire "github.com/veypi/aic-pod/protocol/hosts_tools"
	"net/url"
	"strings"
)

type Config struct {
	Path, StateDir                          string
	Width, Height                           int
	MaxPages                                int
	MaxDownloadBytes, MaxTotalDownloadBytes int64
	MaxUploads                              int
	MaxUploadBytes, MaxTotalUploadBytes     int64
	Logf                                    func(string, ...any)
}
type Empty struct{}

// Stream 是 opaque 双工消息端点（frames/input 流；帧边界 = 消息边界）。
type Stream = wire.Stream
type PageArgs struct {
	PageID string `json:"page_id" required:"true"`
}

func (a *PageArgs) Validate() error {
	if !wire.ValidID(a.PageID) {
		return wire.Fail("invalid_argument", "page_id is required")
	}
	return nil
}

type CreateArgs struct {
	URL    string `json:"url,omitempty"`
	Width  int    `json:"width,omitempty"`
	Height int    `json:"height,omitempty"`
}

func (a *CreateArgs) Validate() error {
	if a.URL != "" {
		if err := validURL(a.URL); err != nil {
			return err
		}
	}
	for _, n := range []int{a.Width, a.Height} {
		if n != 0 && (n < 320 || n > 4096) {
			return wire.Fail("invalid_argument", "Viewport must be 320–4096 pixels")
		}
	}
	return nil
}

type NavigateArgs struct {
	PageID string `json:"page_id" required:"true"`
	URL    string `json:"url" required:"true"`
}

func (a *NavigateArgs) Validate() error {
	if !wire.ValidID(a.PageID) {
		return wire.Fail("invalid_argument", "page_id is required")
	}
	return validURL(a.URL)
}
func validURL(v string) error {
	u, err := url.Parse(v)
	if err != nil || !(u.Scheme == "http" || u.Scheme == "https" || v == "about:blank") {
		return wire.Fail("invalid_argument", "Expected http(s) URL or about:blank")
	}
	return nil
}

type Locator struct {
	Ref   string `json:"ref,omitempty"`
	CSS   string `json:"css,omitempty"`
	Role  string `json:"role,omitempty"`
	Name  string `json:"name,omitempty"`
	Label string `json:"label,omitempty"`
}

func (l Locator) Validate() error {
	n := 0
	for _, s := range []string{l.Ref, l.CSS, l.Role, l.Label} {
		if s != "" {
			n++
		}
	}
	if n != 1 || l.Name != "" && l.Role == "" {
		return wire.Fail("invalid_argument", "locator must contain exactly one of ref, css, role/name or label")
	}
	return nil
}

type ObserveArgs struct {
	PageID string `json:"page_id" required:"true"`
	Image  bool   `json:"image,omitempty"`
	Query  string `json:"query,omitempty"`
	Limit  int    `json:"limit,omitempty"`
}

func (a *ObserveArgs) Validate() error {
	if !wire.ValidID(a.PageID) || a.Limit < 0 || a.Limit > 1000 {
		return wire.Fail("invalid_argument", "Invalid page or observation limit")
	}
	return nil
}

type ActionArgs struct {
	PageID  string  `json:"page_id" required:"true"`
	Locator Locator `json:"locator" required:"true"`
	Text    string  `json:"text,omitempty"`
	Key     string  `json:"key,omitempty"`
	Value   any     `json:"value,omitempty"`
	X       float64 `json:"x,omitempty"`
	Y       float64 `json:"y,omitempty"`
	After   string  `json:"after,omitempty" enum:"none,summary,observation,image"`
}

func (a *ActionArgs) Validate() error {
	if !wire.ValidID(a.PageID) {
		return wire.Fail("invalid_argument", "page_id is required")
	}
	return a.Locator.Validate()
}

type WaitArgs struct {
	PageID    string   `json:"page_id" required:"true"`
	Text      string   `json:"text,omitempty"`
	URL       string   `json:"url,omitempty"`
	Locator   *Locator `json:"locator,omitempty"`
	State     string   `json:"state,omitempty" enum:"visible,hidden,enabled"`
	Load      bool     `json:"load,omitempty"`
	TimeoutMS int64    `json:"timeout_ms,omitempty"`
}

func (a *WaitArgs) Validate() error {
	n := 0
	if a.Text != "" {
		n++
	}
	if a.URL != "" {
		n++
	}
	if a.Load {
		n++
	}
	if a.Locator != nil {
		n++
		if err := a.Locator.Validate(); err != nil {
			return err
		}
	}
	if n != 1 || a.TimeoutMS < 0 || a.TimeoutMS > 300000 {
		return wire.Fail("invalid_argument", "Provide one wait condition and a bounded timeout")
	}
	return nil
}

type DialogArgs struct {
	PageID string `json:"page_id" required:"true"`
	ID     string `json:"dialog_id" required:"true"`
	Accept bool   `json:"accept" required:"true"`
	Text   string `json:"text,omitempty"`
}
type EvaluateArgs struct {
	PageID string `json:"page_id" required:"true"`
	Code   string `json:"code" required:"true"`
}
type EventsArgs struct {
	PageID string `json:"page_id" required:"true"`
	Cursor uint64 `json:"cursor,omitempty"`
	Kind   string `json:"kind,omitempty"`
}
type UploadArgs struct {
	PageID  string  `json:"page_id" required:"true"`
	Locator Locator `json:"locator" required:"true"`
	File    string  `json:"file" required:"true"`
}
type DownloadArgs struct {
	ID   string `json:"download_id" required:"true"`
	Path string `json:"path,omitempty"`
}
type PageInfo struct {
	ID        string  `json:"page_id"`
	Document  string  `json:"document_id"`
	Revision  uint64  `json:"revision"`
	URL       string  `json:"url"`
	Title     string  `json:"title"`
	Width     int     `json:"width"`
	Height    int     `json:"height"`
	Dialog    *Dialog `json:"dialog,omitempty"`
	Uncertain bool    `json:"uncertain,omitempty"`
}
type Dialog struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Message string `json:"message"`
}
type Result struct {
	PageInfo
	Effect   string   `json:"effect"`
	Data     any      `json:"data,omitempty"`
	Warnings []string `json:"warnings,omitempty"`
}
type Element struct {
	Ref   string `json:"ref"`
	Role  string `json:"role"`
	Name  string `json:"name"`
	Value any    `json:"value,omitempty"`
}
type Observation struct {
	PageInfo
	ID        string    `json:"observation_id"`
	Elements  []Element `json:"elements"`
	Image     string    `json:"image,omitempty"`
	MediaType string    `json:"media_type,omitempty"`
	Truncated bool      `json:"truncated,omitempty"`
}
type Event struct {
	Cursor uint64 `json:"cursor"`
	Kind   string `json:"kind"`
	Data   any    `json:"data"`
}
type reference struct {
	backend              int
	role, name, document string
}

// maxRefsPerPage 是单页元素引用容量，只防无限堆积；淘汰替代等待（满容不报错）。
const maxRefsPerPage = 4096

// refTable 是纯序 LRU（无墙钟）：observe 插入到队首，resolve 命中提到队首，
// 满容从队尾淘汰最久未用。引用失效只靠导航检查（document 变化 → stale_ref），
// 不靠时间过期。调用方持有 page.mu。
type refTable struct {
	ll    *list.List // front = 最近使用；Value = *refEntry
	items map[string]*list.Element
}
type refEntry struct {
	id string
	reference
}

func newRefTable() *refTable {
	return &refTable{ll: list.New(), items: map[string]*list.Element{}}
}
func (t *refTable) get(id string) (reference, bool) {
	e, ok := t.items[id]
	if !ok {
		return reference{}, false
	}
	t.ll.MoveToFront(e)
	return e.Value.(*refEntry).reference, true
}
func (t *refTable) put(id string, r reference) {
	if e, ok := t.items[id]; ok {
		e.Value.(*refEntry).reference = r
		t.ll.MoveToFront(e)
		return
	}
	t.items[id] = t.ll.PushFront(&refEntry{id: id, reference: r})
	for t.ll.Len() > maxRefsPerPage {
		back := t.ll.Back()
		t.ll.Remove(back)
		delete(t.items, back.Value.(*refEntry).id)
	}
}

func jsonString(v any) string { b, _ := json.Marshal(v); return string(b) }
func errArg(format string, args ...any) error {
	return wire.Fail("invalid_argument", fmt.Sprintf(format, args...))
}
func bounded(s string, n int) string {
	if len(s) > n {
		return strings.ToValidUTF8(s[:n], "")
	}
	return s
}
