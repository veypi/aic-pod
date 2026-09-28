package vsh

import (
	"testing"

	"github.com/veypi/vsh/commands"
)

// exec 域策略矩阵：白名单（默认）→ 仅种子；open → 未注册名兜底合成；
// deny 恒优先（含 "*" 全禁）。
func TestNativeRegistryPolicy(t *testing.T) {
	t.Parallel()
	n := NewNativeRegistry(NativeDeps{})
	n.Seed("git")
	if !n.IsAllowed("git") || n.IsAllowed("go") {
		t.Fatal("whitelist stance broken")
	}
	if _, ok := n.OpenLookup("go"); ok {
		t.Fatal("whitelist stance must not synthesize")
	}
	n.SetPolicy(true, []string{"rm"})
	if !n.IsAllowed("go") {
		t.Fatal("open stance should allow unregistered")
	}
	if n.IsAllowed("rm") {
		t.Fatal("deny must win over open")
	}
	if _, ok := n.OpenLookup("go"); !ok {
		t.Fatal("open stance should synthesize")
	}
	if _, ok := n.OpenLookup("rm"); ok {
		t.Fatal("denied name must not synthesize")
	}
	if _, ok := n.OpenLookup("a/b"); ok {
		t.Fatal("invalid name must not synthesize")
	}
	n.SetPolicy(true, []string{"*"})
	if n.IsAllowed("git") {
		t.Fatal("deny * must block everything")
	}
}

// fallbackRegistry：base 命中恒优先（D14）；未命中交兜底；Names 只列 base。
func TestFallbackRegistry(t *testing.T) {
	t.Parallel()
	base := commands.NewRegistry()
	_ = base.Register(commands.DefineCommand("hit", nil))
	called := ""
	reg := fallbackRegistry{base: base, fallback: func(name string) (commands.Command, bool) {
		called = name
		if name == "synth" {
			return commands.DefineCommand(name, nil), true
		}
		return nil, false
	}}
	if _, ok := reg.Lookup("hit"); !ok || called != "" {
		t.Fatal("base hit must short-circuit")
	}
	if _, ok := reg.Lookup("synth"); !ok || called != "synth" {
		t.Fatal("miss must consult fallback")
	}
	if _, ok := reg.Lookup("ghost"); ok {
		t.Fatal("fallback false must propagate miss")
	}
	if names := reg.Names(); len(names) != 1 || names[0] != "hit" {
		t.Fatalf("Names = %v, want [hit]", names)
	}
}
