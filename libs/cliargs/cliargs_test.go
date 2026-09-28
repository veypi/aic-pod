package cliargs

import (
	"strings"
	"testing"
)

type createLike struct {
	URL    string `json:"url,omitempty"`
	Width  int    `json:"width,omitempty"`
	Height int    `json:"height,omitempty"`
}

type navLike struct {
	PageID string `json:"page_id"`
	URL    string `json:"url"`
}

// 可选位置参数（"?" 后缀）：缺失放行、提供则按序赋值、旗标混用正常。
func TestOptionalPositional(t *testing.T) {
	t.Parallel()
	// 缺失可选位置参数：放行
	var a createLike
	if err := Parse([]string{"--width", "800"}, []string{"url?"}, &a); err != nil {
		t.Fatal(err)
	}
	if a.URL != "" || a.Width != 800 {
		t.Fatalf("%+v", a)
	}
	// 提供可选位置参数
	var b createLike
	if err := Parse([]string{"about:blank", "--width", "800"}, []string{"url?"}, &b); err != nil {
		t.Fatal(err)
	}
	if b.URL != "about:blank" || b.Width != 800 {
		t.Fatalf("%+v", b)
	}
	// 必需 + 可选混合：必需缺失仍报错
	var c navLike
	if err := Parse(nil, []string{"page_id", "url?"}, &c); err == nil ||
		!strings.Contains(err.Error(), "missing positional argument: page_id") {
		t.Fatalf("required missing must fail: %v", err)
	}
	// 必需齐、可选缺：放行
	var d navLike
	if err := Parse([]string{"p1"}, []string{"page_id", "url?"}, &d); err != nil {
		t.Fatal(err)
	}
	if d.PageID != "p1" || d.URL != "" {
		t.Fatalf("%+v", d)
	}
	// 超出位置参数数量仍报错
	var e createLike
	if err := Parse([]string{"u1", "u2"}, []string{"url?"}, &e); err == nil ||
		!strings.Contains(err.Error(), "unexpected positional argument") {
		t.Fatalf("extra positional must fail: %v", err)
	}
}

// 必需位置参数与旗标解析（回归）。
func TestRequiredPositionalAndFlags(t *testing.T) {
	t.Parallel()
	var a navLike
	if err := Parse([]string{"p1", "https://example.com"}, []string{"page_id", "url"}, &a); err != nil {
		t.Fatal(err)
	}
	if a.PageID != "p1" || a.URL != "https://example.com" {
		t.Fatalf("%+v", a)
	}
	var b navLike
	if err := Parse([]string{"p1"}, []string{"page_id", "url"}, &b); err == nil ||
		!strings.Contains(err.Error(), "missing positional argument: url") {
		t.Fatalf("missing required must fail: %v", err)
	}
}
