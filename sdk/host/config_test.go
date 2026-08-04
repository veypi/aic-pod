package host

import (
	"os"
	"path/filepath"
	"testing"
)

// isolateConfigDir 将配置文件隔离到临时目录（darwin/linux 均经 HOME 或
// XDG_CONFIG_HOME 推导 UserConfigDir；darwin 下 UserConfigDir 用 HOME）。
func isolateConfigDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("HOME", dir)
	return dir
}

func TestConfigLoadDefault(t *testing.T) {
	isolateConfigDir(t)
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Host != DefaultHost {
		t.Fatalf("default host = %q, want %q", cfg.Host, DefaultHost)
	}
	if cfg.Key != "" {
		t.Fatalf("default credential = %q, want empty", cfg.Key)
	}
}

func TestConfigSaveLoadRoundTrip(t *testing.T) {
	isolateConfigDir(t)
	want := Config{
		Host:        "http://localhost:4000",
		Key:         "h1.2.secret.u1",
		WorkDir:     "/workspace",
		ExecTimeout: "5m",
	}
	if err := SaveConfig(want); err != nil {
		t.Fatal(err)
	}
	got, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("round trip = %+v, want %+v", got, want)
	}
	// 文件权限 0600
	p, _ := ConfigPath()
	st, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("config perm = %o, want 600", st.Mode().Perm())
	}
}

func TestConfigSaveNormalizesHost(t *testing.T) {
	isolateConfigDir(t)
	if err := SaveConfig(Config{Host: "  "}); err != nil {
		t.Fatal(err)
	}
	cfg, _ := LoadConfig()
	if cfg.Host != DefaultHost {
		t.Fatalf("normalized host = %q, want %q", cfg.Host, DefaultHost)
	}
}

func TestConfigOptions(t *testing.T) {
	cfg := Config{Host: "https://ivec.ai", Key: "c"}
	opts, err := cfg.Options("cli", "v0.0.1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if opts.Host != "https://ivec.ai" || opts.Key != "c" || opts.DeviceType != "cli" {
		t.Fatalf("options = %+v", opts)
	}
	if opts.ExecTimeout.String() != "30m0s" {
		t.Fatalf("default timeout = %v", opts.ExecTimeout)
	}
	cfg.ExecTimeout = "45m"
	opts, err = cfg.Options("desktop", "v0.0.1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if opts.ExecTimeout.String() != "45m0s" {
		t.Fatalf("timeout = %v", opts.ExecTimeout)
	}
	cfg.ExecTimeout = "bogus"
	if _, err := cfg.Options("cli", "v0.0.1", nil); err == nil {
		t.Fatal("invalid timeout should error")
	}
}

func TestConfigPathIsolated(t *testing.T) {
	dir := isolateConfigDir(t)
	p, err := ConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(p) != filepath.Join(dir, "aic") && filepath.Base(p) != "config.yaml" {
		t.Fatalf("unexpected path %s", p)
	}
}

// 损坏/空配置文件：不阻断启动（flags.LoadCfg 对损坏文件仅 warn，返回当前值）。
func TestLoadConfigCorrupt(t *testing.T) {
	isolateConfigDir(t)
	p, err := ConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("::::broken yaml::::\n[\n"), 0o600); err != nil { // 非法 yaml
		t.Fatal(err)
	}
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("corrupt config should not error: %v", err)
	}
	if cfg.Host != DefaultHost {
		t.Fatalf("host = %q, want default", cfg.Host)
	}
}
