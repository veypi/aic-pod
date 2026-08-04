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
	if cfg.Credential != "" {
		t.Fatalf("default credential = %q, want empty", cfg.Credential)
	}
}

func TestConfigSaveLoadRoundTrip(t *testing.T) {
	isolateConfigDir(t)
	want := Config{
		Host:        "http://localhost:4000",
		Credential:  "h1.2.secret.u1",
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

func TestEnvOverlay(t *testing.T) {
	t.Setenv("AIC_HOST", "http://127.0.0.1:4000")
	t.Setenv("AIC_KEY", "k")
	t.Setenv("AIC_DIR", "/w")
	t.Setenv("AIC_EXEC_TIMEOUT", "1h")
	cfg := EnvOverlay(DefaultConfig())
	if cfg.Host != "http://127.0.0.1:4000" || cfg.Credential != "k" ||
		cfg.WorkDir != "/w" || cfg.ExecTimeout != "1h" {
		t.Fatalf("env overlay = %+v", cfg)
	}
	// 未设置的 env 不覆盖文件值
	os.Unsetenv("AIC_DIR")
	base := Config{Host: "h", WorkDir: "/keep"}
	got := EnvOverlay(base)
	if got.WorkDir != "/keep" {
		t.Fatalf("work_dir overwritten: %+v", got)
	}
}

func TestConfigOptions(t *testing.T) {
	cfg := Config{Host: "https://ivec.ai", Credential: "c"}
	opts, err := cfg.Options("cli", "v0.0.1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if opts.Host != "https://ivec.ai" || opts.Credential != "c" || opts.DeviceType != "cli" {
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
	if filepath.Dir(p) != filepath.Join(dir, "aic") && filepath.Base(p) != "config.json" {
		t.Fatalf("unexpected path %s", p)
	}
}

// 损坏的配置文件：备份为 .bad 并返回默认配置，不阻断启动（进程崩溃截断写入的场景）。
func TestLoadConfigCorrupt(t *testing.T) {
	dir := isolateConfigDir(t)
	p, err := ConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, nil, 0o600); err != nil { // 0 字节半文件
		t.Fatal(err)
	}
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("corrupt config should not error: %v", err)
	}
	if cfg.Host != DefaultHost {
		t.Fatalf("host = %q, want default", cfg.Host)
	}
	if _, err := os.Stat(p + ".bad"); err != nil {
		t.Fatalf("corrupt file not backed up: %v", err)
	}
	_ = dir
}

// Load 在文件读取失败时仍应用 env 覆盖。
func TestLoadEnvOverlayOnError(t *testing.T) {
	isolateConfigDir(t)
	t.Setenv("AIC_HOST", "http://env-host:1")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Host != "http://env-host:1" {
		t.Fatalf("env not applied: %+v", cfg)
	}
}
