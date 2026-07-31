package proto

import (
	"encoding/json"
	"testing"
)

// caps v2 三形态固定向量（§6.3）：缺省/null = 全集或不限制；[] = 不支持/纯虚拟。
// 语义区分依赖 *[]string，Do not "simplify" to []string.

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

func TestCapsProgramsForms(t *testing.T) {
	var c1 Caps // 缺省 = 不限制
	mustUnmarshal(t, `{"host_id":"host_a","exec":{}}`, &c1)
	if !c1.Exec.ProgramsUnrestricted() {
		t.Error("absent programs should be unrestricted")
	}
	var c2 Caps // 显式 null = 不限制
	mustUnmarshal(t, `{"host_id":"host_a","exec":{"programs":null}}`, &c2)
	if !c2.Exec.ProgramsUnrestricted() {
		t.Error("null programs should be unrestricted")
	}
	var c3 Caps // [] = 纯虚拟（browser 类 host）
	mustUnmarshal(t, `{"host_id":"host_a","exec":{"programs":[]}}`, &c3)
	if c3.Exec.ProgramsUnrestricted() || len(*c3.Exec.Programs) != 0 {
		t.Errorf("[] programs = %v, want non-nil empty", c3.Exec.Programs)
	}
	// 序列化保留三形态
	out, _ := json.Marshal(c3)
	var back Caps
	mustUnmarshal(t, string(out), &back)
	if back.Exec.Programs == nil {
		t.Error("[] programs lost after round-trip (became nil)")
	}
}

func TestCapsVirtualDecl(t *testing.T) {
	var c Caps
	mustUnmarshal(t, `{"host_id":"host_a","exec":{"virtual":[
		{"name":"browser","required_level":2,"stateful":true},
		{"name":"bg_kill","required_level":3}
	]}}`, &c)
	if len(c.Exec.Virtual) != 2 {
		t.Fatalf("virtual = %v", c.Exec.Virtual)
	}
	if c.Exec.Virtual[0].Name != "browser" || !c.Exec.Virtual[0].Stateful || c.Exec.Virtual[0].Backgroundable {
		t.Errorf("virtual[0] = %+v", c.Exec.Virtual[0])
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
