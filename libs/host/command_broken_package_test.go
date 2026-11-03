package host

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// 一个坏包（manifest 非法 / 包目录缺失）不能掀掉整张命令注册表：
// 其余包照常注册，坏包在 commands list 里单列一行 rejected。
func TestBrokenPackageDoesNotBreakRegistry(t *testing.T) {
	c, _ := testClient(t)
	root := t.TempDir()
	c.commandPackages = root

	good := filepath.Join(root, "good")
	if err := os.MkdirAll(good, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(good, "manifest.json"),
		[]byte(`{"schema":1,"host":{"good_cmd":{"run":["bin/good"]}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	// 坏包：manifest 是 schema 1 之前的旧结构，严格解析直接拒绝。
	bad := filepath.Join(root, "bad")
	if err := os.MkdirAll(bad, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bad, "manifest.json"),
		[]byte(`{"schema":1,"ui":{"launcher":{"main":{"page":"index"}}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	state := commandPackageState{NextOrder: 4, Packages: map[string]commandPackageRecord{
		"good": {SkillID: "good", Order: 1, Revision: 1},
		"bad":  {SkillID: "bad", Order: 2, Revision: 1},
		"gone": {SkillID: "gone", Order: 3, Revision: 1},
	}}
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "state.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := c.cmds.reload(); err != nil {
		t.Fatalf("a broken package failed the whole registry: %v", err)
	}
	if !c.cmds.has("good_cmd") {
		t.Fatal("healthy package command missing after a broken package")
	}
	broken := c.cmds.brokenPackages()
	if len(broken) != 2 {
		t.Fatalf("broken packages = %+v, want bad + gone", broken)
	}
	if _, ok := broken["gone"]; !ok {
		t.Fatalf("package with missing directory not reported: %+v", broken)
	}

	rows := c.hostCommandRows()
	seen := map[string]hostCommandRow{}
	for _, row := range rows {
		seen[row.Name] = row
	}
	for _, id := range []string{"bad", "gone"} {
		row, ok := seen["package:"+id]
		if !ok {
			t.Fatalf("broken package %s not visible in commands list: %+v", id, rows)
		}
		if row.ID != id || row.Kind != "package" {
			t.Fatalf("broken package row identity = %+v", row)
		}
		if row.State != "rejected" || row.Error == "" {
			t.Fatalf("broken package row = %+v", row)
		}
	}
}

// 坏配置会被反复重试：重试要退避，日志要限频——一份坏配置不该刷出几万行日志。
func TestConfigReloadGateBacksOffAndThrottles(t *testing.T) {
	gate := &configReloadGate{}
	base := time.Now()

	if !gate.shouldTry("f1", base) {
		t.Fatal("first change was not tried")
	}
	if !gate.fail("f1", base) {
		t.Fatal("first failure was not logged")
	}
	// 同一份坏配置：退避期内不重试，日志窗口内不再记。
	if gate.shouldTry("f1", base.Add(time.Second)) {
		t.Fatal("same broken config retried inside the backoff window")
	}
	if gate.fail("f1", base.Add(time.Second)) {
		t.Fatal("same broken config logged again inside the log window")
	}
	// 退避到期：重试；日志窗口未到仍不记。
	if !gate.shouldTry("f1", base.Add(commandConfigRetryInterval+time.Second)) {
		t.Fatal("broken config not retried after the backoff window")
	}
	if gate.fail("f1", base.Add(commandConfigRetryInterval+time.Second)) {
		t.Fatal("broken config logged again inside the log window")
	}
	// 日志窗口到期：再记一次。
	if !gate.fail("f1", base.Add(commandConfigLogInterval+time.Second)) {
		t.Fatal("repeated failure not logged after the log window")
	}
	// 配置内容变了：立刻重试并记日志。
	if !gate.shouldTry("f2", base.Add(2*time.Second)) {
		t.Fatal("changed config was not retried immediately")
	}
	if !gate.fail("f2", base.Add(2*time.Second)) {
		t.Fatal("changed config failure was not logged")
	}
	// 装配成功后同一份内容不再重复装配。
	gate.applied("f2")
	if gate.shouldTry("f2", base.Add(3*time.Second)) {
		t.Fatal("applied config was retried")
	}
}
