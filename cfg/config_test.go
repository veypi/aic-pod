package cfg

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

// isolateConfigDir 在三个桌面平台都隔离配置，避免测试访问用户配置。
func isolateConfigDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("HOME", dir)
	t.Setenv("APPDATA", dir)
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
	// 文件权限 0600
	p, _ := Path()
	st, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && st.Mode().Perm() != 0o600 {
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
	if !st.IsDir() || (runtime.GOOS != "windows" && st.Mode().Perm() != 0o700) {
		t.Fatalf("PublicDir perm = %v isdir=%v, want dir 0700", st.Mode().Perm(), st.IsDir())
	}
	// 幂等：再调不报错
	if _, err := PublicDir(); err != nil {
		t.Fatalf("PublicDir idempotent: %v", err)
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

func TestConfigExecutionPolicyRoundTrip(t *testing.T) {
	isolateConfigDir(t)
	o := &Options{FsPolicy: PolicyDeny, FsRules: []string{"rw:/workspace", "rw:/skills/*/**", "rw:C:/public/**"}, ExecPolicy: PolicyDeny, ExecAllow: []string{"git", "json"}, ExecDeny: []string{"bash"}}
	if err := Save(o); err != nil {
		t.Fatal(err)
	}
	got, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(o.FsRules, got.FsRules) || !reflect.DeepEqual(o.ExecAllow, got.ExecAllow) || !reflect.DeepEqual(o.ExecDeny, got.ExecDeny) || o.FsPolicy != got.FsPolicy || o.ExecPolicy != got.ExecPolicy {
		t.Fatalf("policy round trip: %+v != %+v", AuthFrom(o), AuthFrom(got))
	}
}
func TestInvalidConfigFallsBackWithoutBlockingLoad(t *testing.T) {
	isolateConfigDir(t)
	if err := Save(&Options{FsPolicy: PolicyDeny}); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(); err != nil {
		t.Fatal(err)
	}
	p, _ := Path()
	for _, body := range []string{
		"::::broken yaml::::\n[\n", "fspolicy: open\n", "fs_policy: typo\n", "fs_allow: [ 'ro:' ]\n",
		"fs_allow: [ {path: /, access: rw} ]\n", "fs_deny: [ 'ro:/secret' ]\n", "exec_allow: [ 'git*' ]\n", "net_allow: [ '*:443' ]\n",
		"plain text", "[arbitrary, values]", "", "rtc: invalid\nbrowser_width: [bad]\n",
	} {
		if err := os.WriteFile(p, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		o, err := Load()
		if err != nil {
			t.Fatalf("config must not block startup: %v", err)
		}
		if Global != o || o.Host != DefaultHost || !o.RTC || o.BrowserWidth != 1280 {
			t.Fatalf("missing usable defaults for %q", body)
		}
		if err := o.ValidateAuth(); err != nil && CheckAuth() == nil {
			t.Fatal("malformed authorization did not disable device tools")
		}
		data, _ := os.ReadFile(p)
		if string(data) != body {
			t.Fatal("loading must not overwrite the user's config")
		}
	}
}

func TestConfigIgnoresBadFieldsAndPreservesValidFields(t *testing.T) {
	isolateConfigDir(t)
	p, _ := Path()
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		t.Fatal(err)
	}
	body := "key: existing-device-key\nhost: http://localhost:4000\nhome_path: /agents\nhosts_streams: 4\ncustom: anything\nrtc: typo\nbrowser_width: nope\nhosts_sources: 64\nexec_timeout: invalid\nfs_policy: typo\nfs_rules: ['rw:/workspace']\nexec_allow: [git, {}]\n"
	if err := os.WriteFile(p, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	o, err := LoadFile()
	if err != nil {
		t.Fatal(err)
	}
	if o.Key != "existing-device-key" || o.Host != "http://localhost:4000" || o.HomePath != "/agents" || o.HostsSources != 64 {
		t.Fatal("unrelated invalid fields discarded valid configuration")
	}
	if !o.RTC || o.BrowserWidth != 1280 || o.ExecTimeout != "30m" || o.FsPolicy != "typo" {
		t.Fatal("invalid fields did not fall back to defaults")
	}
	if !reflect.DeepEqual(o.FsRules, []string{"rw:/workspace"}) || o.ValidateAuth() == nil {
		t.Fatal("valid rule lost or malformed authorization became valid")
	}
}

// TestMalformedAuthorizationDoesNotBlockSave：坏授权（含整体解析失败）不再是保存门——
// 保存永远落盘（工具仍 fail-closed）；坏内容以原样或 INVALID 标记留在文件里，可见可修。
func TestMalformedAuthorizationDoesNotBlockSave(t *testing.T) {
	isolateConfigDir(t)
	saved := Global
	t.Cleanup(func() { Global = saved })
	p, _ := Path()
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		body  string
		gated bool   // ValidateAuth 仍失败（工具 fail-closed）
		keep  string // 保存后仍应可见的片段（空 = 不检查）
	}{
		{"exec_policy: dney\nexec_deny: [sh]\n", true, "dney"},
		{"exec_policy: open\nexec_deny: [sh, 'bad rule']\n", true, "bad rule"},
		{"exec_policy: open\nexec_deny: [sh, {}]\n", true, "INVALID exec_deny"},
		{"[broken yaml\n", true, "INVALID"},
		{"fs_policy: open\nfs_deny: [/private, 'ro:/secret']\n", false, ""}, // 旧键直接失效
		{"net_policy: open\nnet_deny: [example.com, '*:443']\n", false, ""},
		{"ssh_policy: open\nssh_allow: [example.com, '*:22']\n", false, ""},
	} {
		t.Run(tc.body, func(t *testing.T) {
			if err := os.WriteFile(p, []byte(tc.body), 0600); err != nil {
				t.Fatal(err)
			}
			o, err := Load()
			if err != nil {
				t.Fatal(err)
			}
			o.Normalize() // repeated startup normalization must not erase the error
			if gated := CheckAuth() != nil; gated != tc.gated {
				t.Fatalf("CheckAuth gated = %v, want %v (%v)", gated, tc.gated, CheckAuth())
			}
			o.HomePath = "/agents"
			if err := Save(o); err != nil {
				t.Fatalf("save must land regardless of file content: %v", err)
			}
			data, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(data), "home_path: /agents") {
				t.Fatalf("save did not land: %s", data)
			}
			if tc.keep != "" && !strings.Contains(string(data), tc.keep) {
				t.Fatalf("malformed content lost (want %q): %s", tc.keep, data)
			}
			// 重载：坏内容没有被静默修好（门控状态一致）
			if _, err := Load(); err != nil {
				t.Fatal(err)
			}
			if gated := CheckAuth() != nil; gated != tc.gated {
				t.Fatalf("CheckAuth after reload = %v, want %v", gated, tc.gated)
			}
		})
	}
	o := NewOptions()
	o.ExecPolicy, o.ExecDeny = PolicyOpen, []string{"sh", "bad rule"}
	o.Normalize()
	if !reflect.DeepEqual(o.ExecDeny, []string{"sh", "bad rule"}) {
		t.Fatal("deny entries were discarded")
	}
}

// TestDeprecatedAuthKeysIgnoredAndDroppedOnSave：规则表化废弃键直接失效——
// 加载忽略（不迁移），保存永远落盘，旧键随重写自然清除。
func TestDeprecatedAuthKeysIgnoredAndDroppedOnSave(t *testing.T) {
	isolateConfigDir(t)
	p, _ := Path()
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		t.Fatal(err)
	}
	body := "fs_policy: deny\nfs_deny: [/private/**]\nfs_allow: [/work]\nnet_allow: [example.com:443]\n"
	if err := os.WriteFile(p, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	o, err := LoadFile()
	if err != nil {
		t.Fatal(err)
	}
	if len(o.FsRules) != 0 || o.ValidateAuth() != nil {
		t.Fatalf("deprecated keys must be ignored: %+v (%v)", o, o.ValidateAuth())
	}
	o.HomePath = "/agents"
	if err := Save(o); err != nil {
		t.Fatalf("save must land regardless of file content: %v", err)
	}
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"fs_deny", "fs_allow", "net_allow"} {
		if strings.Contains(string(data), key+":") {
			t.Fatalf("deprecated key %s survived rewrite: %s", key, data)
		}
	}
	o2, err := LoadFile()
	if err != nil || o2.HomePath != "/agents" || o2.ValidateAuth() != nil {
		t.Fatalf("rewritten config should be clean: %+v (%v)", o2, err)
	}
}

func TestUnreadableConfigFallsBack(t *testing.T) {
	isolateConfigDir(t)
	p, _ := Path()
	// 配置路径为目录，模拟读文件失败（root 运行时 chmod 仍可能可读）。
	if err := os.MkdirAll(p, 0700); err != nil {
		t.Fatal(err)
	}
	o, err := Load()
	if err != nil || o.Host != DefaultHost {
		t.Fatalf("unreadable config blocked startup: %v", err)
	}
}
