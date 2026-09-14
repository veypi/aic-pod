package cfg

import (
	"os"
	"path/filepath"
	"strings"
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

// PublicDir 返回 $HOME/.aic 并创建（0700）。
func TestPublicDir(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	p, err := PublicDir()
	if err != nil {
		t.Fatal(err)
	}
	if p != filepath.Join(dir, ".aic") {
		t.Fatalf("PublicDir = %q, want %q", p, filepath.Join(dir, ".aic"))
	}
	st, err := os.Stat(p)
	if err != nil {
		t.Fatalf("PublicDir not created: %v", err)
	}
	if !st.IsDir() || st.Mode().Perm() != 0o700 {
		t.Fatalf("PublicDir perm = %v isdir=%v, want dir 0700", st.Mode().Perm(), st.IsDir())
	}
	// 幂等：再调不报错
	if _, err := PublicDir(); err != nil {
		t.Fatalf("PublicDir idempotent: %v", err)
	}
}

func TestHostsURL(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", "https://ivec-ai.com/hosts"},
		{"https://ivec-ai.com", "https://ivec-ai.com/hosts"},
		{"http://localhost:4000", "http://localhost:4000/hosts"},
		{"http://localhost:4000/", "http://localhost:4000/hosts"},
		{"http://127.0.0.1:4000/rses/aiv", "http://127.0.0.1:4000/rses/aiv/hosts"},
		{"https://ivec-ai.com/hosts", "https://ivec-ai.com/hosts"},
		{"ivec-ai.com", "https://ivec-ai.com/hosts"},
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
		{"", "", "https://ivec-ai.com/"},
		{"https://ivec-ai.com", "", "https://ivec-ai.com/"},
		{"https://ivec-ai.com", "/a", "https://ivec-ai.com/a"},
		{"http://localhost:4000/", "/", "http://localhost:4000/"},
		{"http://127.0.0.1:4000/rses/aiv", "/agents", "http://127.0.0.1:4000/rses/aiv/agents"},
		{"ivec-ai.com", "/a", "https://ivec-ai.com/a"},
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
		{"a", "/a"},         // 缺斜杠自动补
		{"//evil.com", "/"}, // 协议相对 URL 形态 → 拒绝回退
		{" /a ", "/a"},      // 去空白
	}
	for _, c := range cases {
		o := &Options{HomePath: c.in}
		if got := o.NormalizedHomePath(); got != c.want {
			t.Errorf("NormalizedHomePath(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// 配置文件键形态：yaml tag（snake_case，与本地 API/文档同名）为主形态，历史
// 小写字段名（fspolicy/workdir/fsdeny…）兼容读入；两形态同现时 snake_case 胜出；
// Save 落盘为 snake_case（旧文件自愈）。
func TestConfigYAMLKeyForms(t *testing.T) {
	isolateConfigDir(t)
	p, err := Path()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	// snake_case 形态（文档/本地 API 形态）
	body := "host: http://localhost:4000\nwork_dir: /ws\nfs_policy: open\nfs_allow:\n  - /ws/**/.env\nno_sandbox: true\n"
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	o, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if o.Host != "http://localhost:4000" || o.WorkDir != "/ws" || o.FsPolicy != PolicyOpen || !o.NoSandbox ||
		len(o.FsAllow) != 1 || o.FsAllow[0] != "/ws/**/.env" {
		t.Fatalf("snake_case config not parsed: %+v", *o)
	}
	// 历史小写字段名形态（兼容读入）
	legacy := "host: http://x:1\nworkdir: /legacy\nexectimeout: 9m\nfspolicy: deny\nfsallow:\n  - /legacy/allow\nfsdeny:\n  - '**/*.pem'\n"
	if err := os.WriteFile(p, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	o, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if o.Host != "http://x:1" || o.WorkDir != "/legacy" || o.ExecTimeout != "9m" || o.FsPolicy != PolicyDeny ||
		len(o.FsAllow) != 1 || o.FsAllow[0] != "/legacy/allow" || len(o.FsDeny) != 1 || o.FsDeny[0] != "**/*.pem" {
		t.Fatalf("legacy lowercased config not parsed: %+v", *o)
	}
	// 两形态同现：yaml tag 形态胜出
	both := "host: http://x:1\nwork_dir: /snake\nworkdir: /legacy\n"
	if err := os.WriteFile(p, []byte(both), 0o600); err != nil {
		t.Fatal(err)
	}
	o, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if o.WorkDir != "/snake" {
		t.Fatalf("snake_case must win over legacy key: %q", o.WorkDir)
	}
	// Save 落盘为 snake_case（自愈）
	if err := Save(o); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "work_dir: /snake") || strings.Contains(string(b), "workdir:") {
		t.Fatalf("Save must write snake_case keys:\n%s", b)
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
