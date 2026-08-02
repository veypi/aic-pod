package proto

import (
	"encoding/json"
	"strings"
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
		{"name":"browser","desc":"control a web browser","help":"browser <sub>","level":2},
		{"name":"bg_kill","level":3},
		{"name":"unknown_level"}
	]}}`, &c)
	if len(c.Exec.Virtual) != 3 {
		t.Fatalf("virtual = %v", c.Exec.Virtual)
	}
	v0 := c.Exec.Virtual[0]
	if v0.Name != "browser" || v0.Desc != "control a web browser" || v0.Help != "browser <sub>" || v0.RequiredLevel != 2 {
		t.Errorf("virtual[0] = %+v", v0)
	}
	if c.Exec.Virtual[1].RequiredLevel != 3 {
		t.Errorf("virtual[1] level = %d", c.Exec.Virtual[1].RequiredLevel)
	}
	// 未声明 level = 0（不暴露给 AI，仅供服务端按需处理；host 必备指令都会显式声明）
	if c.Exec.Virtual[2].RequiredLevel != 0 {
		t.Errorf("virtual[2] level = %d, want 0", c.Exec.Virtual[2].RequiredLevel)
	}
	// 序列化：level 非零必出；desc/help 非空必出；零值省略
	out, _ := json.Marshal(c.Exec.Virtual[0])
	if !strings.Contains(string(out), "\"level\":2") || !strings.Contains(string(out), "\"desc\"") || !strings.Contains(string(out), "\"help\"") {
		t.Errorf("virtual[0] marshal = %s", out)
	}
	out2, _ := json.Marshal(c.Exec.Virtual[2])
	if strings.Contains(string(out2), "\"level\"") || strings.Contains(string(out2), "desc") {
		t.Errorf("virtual[2] marshal should omit zero fields: %s", out2)
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
