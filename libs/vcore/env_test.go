package vcore

import (
	"strings"
	"testing"
)

// stubPolicy 以固定分级回答 Decide（测试用）。
type stubPolicy struct{ r, w int }

func (p stubPolicy) Decide(string) (int, int) { return p.r, p.w }

// noFollowStub 实现 NoFollowPolicy：记录 unlink 语义入口的调用次数。
type noFollowStub struct {
	stubPolicy
	noFollowCalls int
}

func (p *noFollowStub) DecideNoFollow(string) (int, int) {
	p.noFollowCalls++
	return p.r, p.w
}

// CheckPolicyUnlink：实现 NoFollowPolicy 的策略走 DecideNoFollow；未实现者
// （stubPolicy）回退 Decide；两档拒绝文案与 deny 优先语义不变。
func TestCheckPolicyUnlinkUsesNoFollow(t *testing.T) {
	nf := &noFollowStub{stubPolicy: stubPolicy{1, 2}}
	env := &Env{Policy: nf, Granted: 2}
	if err := env.CheckPolicyUnlink("fs rm", "/ws/link", true); err != nil {
		t.Fatalf("no-follow write denied: %v", err)
	}
	if nf.noFollowCalls != 1 {
		t.Fatalf("DecideNoFollow calls = %d, want 1", nf.noFollowCalls)
	}
	// 回退：未实现 NoFollowPolicy 的策略仍按 Decide 判定。
	env2 := &Env{Policy: stubPolicy{1, 0}, Granted: 0}
	if err := env2.CheckPolicyUnlink("fs rm", "/x", true); err == nil || !strings.Contains(err.Error(), "request access with exec grant") {
		t.Fatalf("fallback must keep grantable denial, got %v", err)
	}
	// deny 命中不受入口影响（读 0 级，grant 不可绕过）。
	env3 := &Env{Policy: &noFollowStub{stubPolicy: stubPolicy{0, 0}}, Granted: 9}
	if err := env3.CheckPolicyUnlink("fs rm", "/x", true); err == nil || !strings.Contains(err.Error(), "fs_deny list") {
		t.Fatalf("deny must stay absolute: %v", err)
	}
}

// CheckPolicy 的两档拒绝文案必须区分：deny 命中（读 0 级，本地规则不可 grant）
// 与等级不足（白名单外写/等级不够，可经 grant 或审批放行）——对 deny 目标
// 提示 grant 是无效且误导的，反向也不得把可 grant 的拒绝说成 fs_deny。
func TestCheckPolicyDenyVsInsufficientGrant(t *testing.T) {
	for _, tc := range []struct {
		name    string
		policy  stubPolicy
		write   bool
		granted int
		want    string
	}{
		{"deny-read", stubPolicy{0, 0}, false, 9, "fs_deny list"},
		{"deny-write", stubPolicy{0, 0}, true, 9, "fs_deny list"},
		{"write-outside-allow", stubPolicy{1, 0}, true, 0, "request access with exec grant"},
		{"read-above-grant", stubPolicy{1, 0}, false, 0, "request access with exec grant"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := &Env{Policy: tc.policy, Granted: tc.granted}
			err := env.CheckPolicy("fs read", "/x", tc.write)
			if err == nil {
				t.Fatal("expected denial")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q missing %q", err.Error(), tc.want)
			}
			if tc.want == "fs_deny list" {
				if strings.Contains(err.Error(), "request access with exec grant") {
					t.Fatalf("deny message must not suggest an impossible grant: %q", err.Error())
				}
			} else if strings.Contains(err.Error(), "fs_deny list") {
				t.Fatalf("grantable denial must not claim fs_deny: %q", err.Error())
			}
		})
	}
	// 放行档：granted >= need 不报错。
	if err := (&Env{Policy: stubPolicy{1, 2}, Granted: 2}).CheckPolicy("fs write", "/x", true); err != nil {
		t.Fatalf("granted write denied: %v", err)
	}
}
