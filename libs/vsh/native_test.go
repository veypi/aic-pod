package vsh

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/veypi/aic-pod/libs/exec_procs"
	"github.com/veypi/vsh/commands"
)

func TestNativeWhitelistGating(t *testing.T) {
	t.Parallel()
	nr := NewNativeRegistry(NativeDeps{})
	nr.Seed("ffmpeg", "python3")
	if !nr.IsAllowed("ffmpeg") || !nr.IsAllowed("python3") {
		t.Fatal("seed failed")
	}
	if nr.IsAllowed("bash") {
		t.Fatal("bash must not be seeded（默认白名单不含 shell/解释器——bash 是引擎内建）")
	}
	reg := commands.NewRegistry()
	if err := nr.RegisterInto(reg); err != nil {
		t.Fatal(err)
	}
	if _, ok := reg.Lookup("ffmpeg"); !ok {
		t.Fatal("ffmpeg not registered")
	}
	if _, ok := reg.Lookup("bash"); ok {
		t.Fatal("bash must stay unregistered（白名单外 = 引擎 127）")
	}
	// 白名单外注册被拒。
	if err := nr.Register(reg, "curl-native"); err == nil {
		t.Fatal("register outside whitelist should fail")
	}
	// grant cmd 扩充后可注册。
	nr.Allow("curl-native")
	if err := nr.Register(reg, "curl-native"); err != nil {
		t.Fatal(err)
	}
}

func TestNativeCommandRunsThroughManager(t *testing.T) {
	t.Parallel()
	m := exec_procs.NewManager(30 * time.Second)
	defer m.Close(context.Background())
	nr := NewNativeRegistry(NativeDeps{
		Manager: m,
		Policy: func() NativePolicy {
			// NoSandbox：单测不依赖沙箱后端（沙箱收容是 exec_procs 的既有
			// 测试面，验收 10 在 M4 端到端做）。
			return NativePolicy{NoSandbox: true}
		},
	})
	nr.Seed("echo")
	reg := commands.NewRegistry()
	if err := nr.RegisterInto(reg); err != nil {
		t.Fatal(err)
	}
	cmd, _ := reg.Lookup("echo")
	var out bytes.Buffer
	err := commands.RunCommand(context.Background(), cmd, &commands.Invocation{
		Args:   []string{"native-hello"},
		Stdout: &out,
		Env:    map[string]string{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out.String()) != "native-hello" {
		t.Fatalf("out = %q", out.String())
	}
}

func TestNativeExitCodePassthrough(t *testing.T) {
	t.Parallel()
	m := exec_procs.NewManager(30 * time.Second)
	defer m.Close(context.Background())
	nr := NewNativeRegistry(NativeDeps{
		Manager: m,
		Policy:  func() NativePolicy { return NativePolicy{NoSandbox: true} },
	})
	nr.Seed("false")
	reg := commands.NewRegistry()
	_ = nr.RegisterInto(reg)
	cmd, _ := reg.Lookup("false")
	err := commands.RunCommand(context.Background(), cmd, &commands.Invocation{
		Stdout: &bytes.Buffer{}, Env: map[string]string{},
	})
	code, ok := commands.ExitCode(err)
	if !ok || code == 0 {
		t.Fatalf("exit = %v (%d,%v)", err, code, ok)
	}
}
