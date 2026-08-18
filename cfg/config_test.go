package cfg

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
	o, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if o.Host != DefaultHost {
		t.Fatalf("default host = %q, want %q", o.Host, DefaultHost)
	}
	if o.Key != "" {
		t.Fatalf("default credential = %q, want empty", o.Key)
	}
	if o.HomePath != "/" {
		t.Fatalf("default home_path = %q, want /", o.HomePath)
	}
	if Global != o {
		t.Fatal("Load should set Global")
	}
}

func TestConfigSaveLoadRoundTrip(t *testing.T) {
	isolateConfigDir(t)
	want := Options{
		Host:        "http://localhost:4000",
		Key:         "h1.2.secret.u1",
		WorkDir:     "/workspace",
		ExecTimeout: "5m",
		HomePath:    "/a",
	}
	if err := Save(&want); err != nil {
		t.Fatal(err)
	}
	got, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	// 导出字段逐项比较（port 为进程级隐私字段；code 随机生成不落盘，不参与比较）
	if got.Host != want.Host || got.Key != want.Key || got.WorkDir != want.WorkDir || got.ExecTimeout != want.ExecTimeout || got.HomePath != want.HomePath {
		t.Fatalf("round trip = %+v, want %+v", *got, want)
	}
	if got.Code == "" {
		t.Fatal("code should be generated when unset")
	}
	// 文件权限 0600
	p, _ := Path()
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
	if err := Save(&Options{Host: "  "}); err != nil {
		t.Fatal(err)
	}
	o, _ := Load()
	if o.Host != DefaultHost {
		t.Fatalf("normalized host = %q, want %q", o.Host, DefaultHost)
	}
}

func TestConfigPathIsolated(t *testing.T) {
	dir := isolateConfigDir(t)
	p, err := Path()
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(p) != filepath.Join(dir, "aic") && filepath.Base(p) != "config.yaml" {
		t.Fatalf("unexpected path %s", p)
	}
}

func TestHostsURL(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", "https://ivec.ai/hosts"},
		{"https://ivec.ai", "https://ivec.ai/hosts"},
		{"http://localhost:4000", "http://localhost:4000/hosts"},
		{"http://localhost:4000/", "http://localhost:4000/hosts"},
		{"http://127.0.0.1:4000/rses/aiv", "http://127.0.0.1:4000/rses/aiv/hosts"},
		{"https://ivec.ai/hosts", "https://ivec.ai/hosts"},
		{"ivec.ai", "https://ivec.ai/hosts"},
		{"http://x:1/?q=1", "http://x:1/hosts"},
	}
	for _, c := range cases {
		o := &Options{Host: c.in}
		if got := o.HostsURL(); got != c.want {
			t.Errorf("HostsURL(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestHomeURL(t *testing.T) {
	cases := []struct{ host, home, want string }{
		{"", "", "https://ivec.ai/"},
		{"https://ivec.ai", "", "https://ivec.ai/"},
		{"https://ivec.ai", "/a", "https://ivec.ai/a"},
		{"http://localhost:4000/", "/", "http://localhost:4000/"},
		{"http://127.0.0.1:4000/rses/aiv", "/agents", "http://127.0.0.1:4000/rses/aiv/agents"},
		{"ivec.ai", "/a", "https://ivec.ai/a"},
		{"http://x:1/?q=1", "/a", "http://x:1/a"},
	}
	for _, c := range cases {
		o := &Options{Host: c.host, HomePath: c.home}
		if got := o.HomeURL(); got != c.want {
			t.Errorf("HomeURL(%q, %q) = %q, want %q", c.host, c.home, got, c.want)
		}
	}
}

func TestNormalizedHomePath(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", "/"},
		{"  ", "/"},
		{"/", "/"},
		{"/a", "/a"},
		{"/agents/chat", "/agents/chat"},
		{"a", "/a"},     // 缺斜杠自动补
		{"//evil.com", "/"}, // 协议相对 URL 形态 → 拒绝回退
		{" /a ", "/a"},  // 去空白
	}
	for _, c := range cases {
		o := &Options{HomePath: c.in}
		if got := o.NormalizedHomePath(); got != c.want {
			t.Errorf("NormalizedHomePath(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// 损坏/空配置文件：不阻断启动（flags.LoadCfg 对损坏文件仅 warn，返回当前值）。
func TestLoadConfigCorrupt(t *testing.T) {
	isolateConfigDir(t)
	p, err := Path()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("::::broken yaml::::\n[\n"), 0o600); err != nil { // 非法 yaml
		t.Fatal(err)
	}
	o, err := Load()
	if err != nil {
		t.Fatalf("corrupt config should not error: %v", err)
	}
	if o.Host != DefaultHost {
		t.Fatalf("host = %q, want default", o.Host)
	}
}
