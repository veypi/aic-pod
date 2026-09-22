package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/veypi/aic-pod/cfg"
)

// capture 捕获 fn 的 stdout 输出（设置面子命令走 stdout 返回 JSON）。
func capture(t *testing.T, fn func() error) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	runErr := fn()
	_ = w.Close()
	os.Stdout = old
	b, _ := io.ReadAll(r)
	_ = r.Close()
	if runErr != nil {
		t.Fatalf("subcommand failed: %v", runErr)
	}
	return string(b)
}

// withStdin 以 in 作为 stdin 执行 fn（凭证/设置 JSON 走 stdin）。
func withStdin(t *testing.T, in string, fn func() error) error {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "stdin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(in); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	old := os.Stdin
	os.Stdin = f
	defer func() { os.Stdin = old; _ = f.Close() }()
	return fn()
}

// isolateConfigDir 在三个桌面平台都隔离配置目录（同 cfg 包测试口径）。
func isolateConfigDir(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("HOME", dir)
	t.Setenv("APPDATA", dir)
}

// writeRawConfig 写入原始配置文本（可为坏 YAML）。
func writeRawConfig(t *testing.T, body string) {
	t.Helper()
	p, err := cfg.Path()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// repairedSettings 是全量修复用的设置 JSON（清空四域授权列表 + 指定 host/home_path）。
const repairedSettings = `{"host":"http://localhost:4000","home_path":"/agents","exec_policy":"deny","exec_allow":[],"exec_deny":[],"fs_policy":"deny","fs_rules":[],"net_policy":"deny","net_rules":[],"ssh_policy":"deny","ssh_rules":[]}`

// TestInvalidConfigStillLoadsAndCanBeRepaired：坏配置不得阻断启动与修复——
// `config get` 仍能回显（坏授权字段显式可见），`config set` 能修好并落盘
// （原 HTTP 版用例的替代：本地已无管理 API，设置面 = config.yaml + 子命令）。
func TestInvalidConfigStillLoadsAndCanBeRepaired(t *testing.T) {
	for index, body := range []string{
		"hosts_streams: 4\nextra_parameter: anything\n",
		"rtc: bad\nbrowser_width: [wrong]\nexec_policy: wrong\n",
		"[completely broken yaml\n",
		"unknown: junk\n",
	} {
		t.Run(fmt.Sprintf("config%d", index), func(t *testing.T) {
			isolateConfigDir(t)
			writeRawConfig(t, body)
			// config get：与真实 CLI 同口径（flags.Parse 装载 Global 后输出视图）
			if _, err := cfg.Load(); err != nil {
				t.Fatal(err)
			}
			out := capture(t, runConfigGet)
			var view map[string]any
			if err := json.Unmarshal([]byte(out), &view); err != nil {
				t.Fatalf("config get output is not JSON: %v (%q)", err, out)
			}
			if view["host"] == nil {
				t.Fatalf("config get missing host: %q", out)
			}
			// config set：修复并落盘
			if err := withStdin(t, repairedSettings, runConfigSet); err != nil {
				t.Fatal(err)
			}
			repaired, err := cfg.LoadFile()
			if err != nil || repaired.Host != "http://localhost:4000" || repaired.HomePath != "/agents" {
				t.Fatalf("configuration repair not persisted: %v", err)
			}
		})
	}
}

// TestConfigSetRejectsBadValues：非法值必须拒绝且不落盘。
func TestConfigSetRejectsBadValues(t *testing.T) {
	for _, body := range []string{
		`{"fs_policy":"typo"}`,
		`{"exec_timeout":"not-a-duration"}`,
		`{"browser_width":100}`,
		`{"home_path":"//evil.example/"}`,
		`{"work_dir":"/definitely/not/a/directory"}`,
	} {
		t.Run(body, func(t *testing.T) {
			isolateConfigDir(t)
			writeRawConfig(t, "host: https://ivec-ai.com\n")
			p, _ := cfg.Path()
			before, _ := os.ReadFile(p)
			if err := withStdin(t, body, runConfigSet); err == nil {
				t.Fatalf("invalid settings accepted: %s", body)
			}
			after, _ := os.ReadFile(p)
			if string(before) != string(after) {
				t.Fatal("invalid settings were persisted")
			}
		})
	}
}

// TestBindUnbindRoundTrip：bind 写入凭证（含 host_id 解析），unbind 清空凭证。
func TestBindUnbindRoundTrip(t *testing.T) {
	isolateConfigDir(t)
	writeRawConfig(t, "host: https://ivec-ai.com\n")
	const cred = "ab62a0c624f2496b8e7cf8f63c735d1d.aabbccdd.1700000000.0123456789abcdef"
	out := capture(t, func() error { return withStdin(t, cred+"\n", runBind) })
	var resp struct {
		OK     bool   `json:"ok"`
		HostID string `json:"host_id"`
	}
	if err := json.Unmarshal([]byte(out), &resp); err != nil || !resp.OK {
		t.Fatalf("bind output: %q (%v)", out, err)
	}
	if resp.HostID != "ab62a0c624f2496b8e7cf8f63c735d1d" {
		t.Fatalf("host_id = %q", resp.HostID)
	}
	got, err := cfg.LoadFile()
	if err != nil || got.Key != cred {
		t.Fatalf("credential not persisted: %v", err)
	}
	capture(t, runUnbind)
	got, err = cfg.LoadFile()
	if err != nil || got.Key != "" {
		t.Fatalf("credential not cleared: %v", err)
	}
	// 空凭证必须拒绝
	if err := withStdin(t, "   \n", runBind); err == nil {
		t.Fatal("empty credential accepted")
	}
}

// TestSaveLandsDespiteLegacyOrBrokenFile：文件内容不构成保存门——旧键/坏 YAML 下
// bind 与 config set 都照常落盘，旧键随重写自然清除。
func TestSaveLandsDespiteLegacyOrBrokenFile(t *testing.T) {
	for index, body := range []string{
		"host: http://localhost:4000\nfs_deny: [/private/**]\nfs_allow: [/work]\nssh_allow: [example.com:22]\n",
		"[completely broken yaml\n",
	} {
		t.Run(fmt.Sprintf("config%d", index), func(t *testing.T) {
			isolateConfigDir(t)
			writeRawConfig(t, body)
			const cred = "ab62a0c624f2496b8e7cf8f63c735d1d.aabbccdd.1700000000.0123456789abcdef"
			capture(t, func() error { return withStdin(t, cred+"\n", runBind) })
			bound, err := cfg.LoadFile()
			if err != nil || bound.Key != cred {
				t.Fatalf("bind must land regardless of file content: %+v (%v)", bound, err)
			}
			if err := withStdin(t, `{"home_path":"/agents"}`, runConfigSet); err != nil {
				t.Fatalf("unrelated save must land: %v", err)
			}
			got, err := cfg.LoadFile()
			if err != nil || got.HomePath != "/agents" || got.Key != cred {
				t.Fatalf("save not persisted: %+v (%v)", got, err)
			}
			if index != 0 {
				return
			}
			p, _ := cfg.Path()
			data, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(data), "fs_deny") || strings.Contains(string(data), "ssh_allow") {
				t.Fatalf("legacy keys should be dropped by rewrite: %s", data)
			}
		})
	}
}
