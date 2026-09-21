package proto

import (
	"encoding/json"
	"strings"
	"testing"
)

// caps v2 固定向量（§6.3）：fs.actions 三形态（缺省/null = 全集；[] = 不支持）
// 语义区分依赖 *[]string，Do not "simplify" to []string。
// exec.commands 为统一命令声明表：未声明的命令一律拒绝（无三形态语义）。

func TestCapsFSActionsForms(t *testing.T) {
	// 字段缺省
	var c1 Caps
	mustUnmarshal(t, `{"host_id":"host_a","fs":{}}`, &c1)
	if c1.FS.Actions != nil {
		t.Errorf("absent actions = %v, want nil", *c1.FS.Actions)
	}
	// 显式 null
	var c2 Caps
	mustUnmarshal(t, `{"host_id":"host_a","fs":{"actions":null}}`, &c2)
	if c2.FS.Actions != nil {
		t.Errorf("null actions = %v, want nil", *c2.FS.Actions)
	}
	// 空数组 = 不支持 fs
	var c3 Caps
	mustUnmarshal(t, `{"host_id":"host_a","fs":{"actions":[]}}`, &c3)
	if c3.FS.Actions == nil || len(*c3.FS.Actions) != 0 {
		t.Errorf("[] actions = %v, want non-nil empty", c3.FS.Actions)
	}
	if !c1.FS.Supports("read") || !c1.FS.Supports("write") || !c1.FS.Supports("edit") {
		t.Error("nil actions should support all 3")
	}
	if c3.FS.Supports("read") {
		t.Error("[] actions should support none")
	}
	// 子集
	var c4 Caps
	mustUnmarshal(t, `{"host_id":"host_a","fs":{"actions":["read"]}}`, &c4)
	if !c4.FS.Supports("read") || c4.FS.Supports("write") {
		t.Errorf("subset actions wrong: %v", *c4.FS.Actions)
	}
}

func TestCapsCommandDecl(t *testing.T) {
	var c Caps
	mustUnmarshal(t, `{"host_id":"host_a","exec":{"epoch":"runtime_epoch","commands":[
 {"name":"browser","desc":"control a web browser","help":"browser <method>","level":1,"methods":[{"name":"page.wait","mode":"call","access":1,"background":true,"input":{"type":"object"}}]},
 {"name":"sh","raw_argv":true,"level":3,"methods":[{"name":"run","mode":"call","access":3,"background":true,"input":{"type":"object"}}]}
 ]}}`, &c)
	if c.Exec.Epoch != "runtime_epoch" || len(c.Exec.Commands) != 2 {
		t.Fatal(c.Exec)
	}
	browser := c.Exec.Commands[0]
	if browser.RequiredLevel != 1 || browser.Help == "" || len(browser.Methods) != 1 || !browser.Methods[0].Background {
		t.Fatal(browser)
	}
	shell := c.Exec.Commands[1]
	if !shell.RawArgv || shell.RequiredLevel != 3 || shell.Methods[0].Name != "run" {
		t.Fatal(shell)
	}
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	var roundtrip Caps
	mustUnmarshal(t, string(raw), &roundtrip)
	if string(roundtrip.Exec.Commands[0].Methods[0].Input) != `{"type":"object"}` || roundtrip.Exec.Epoch != c.Exec.Epoch {
		t.Fatal("method schema or execution epoch lost")
	}
	if strings.Contains(string(raw), `"tools":`) {
		t.Fatal("parallel tool capability published")
	}
}

func TestAgentVersion(t *testing.T) {
	ok := map[string][3]int{
		"v0.3.0":        {0, 3, 0},
		"v1.0.0":        {1, 0, 0},
		"v2.12.3-rc.1":  {2, 12, 3},
		"v10.20.30-dev": {10, 20, 30},
	}
	for v, want := range ok {
		a, b, c, err := ParseAgentVersion(v)
		if err != nil || [3]int{a, b, c} != want {
			t.Errorf("ParseAgentVersion(%q) = %d.%d.%d, %v", v, a, b, c, err)
		}
	}
	for _, v := range []string{"", "0.3.0", "v0.3", "va.b.c", "v1.2.3.4", "v1..2"} {
		if _, _, _, err := ParseAgentVersion(v); err == nil {
			t.Errorf("ParseAgentVersion(%q) want error", v)
		}
	}
	if !MajorVersionMatch("v0.3.0", "v0.9.1") {
		t.Error("same major should match")
	}
	if MajorVersionMatch("v0.3.0", "v1.0.0") {
		t.Error("different major should not match")
	}
	if MajorVersionMatch("bad", "v1.0.0") {
		t.Error("invalid version should not match")
	}
}

func mustUnmarshal(t *testing.T, s string, v any) {
	t.Helper()
	if err := json.Unmarshal([]byte(s), v); err != nil {
		t.Fatal(err)
	}
}
