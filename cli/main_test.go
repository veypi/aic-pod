package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/veypi/aic-pod/cfg"
)

func TestInvalidConfigStillStartsAndCanBeRepaired(t *testing.T) {
	if os.Getenv("AIC_TEST_CONFIG_STARTUP") == "1" {
		os.Args = os.Args[:1]
		main()
		return
	}
	for index, body := range []string{
		"hosts_streams: 4\nextra_parameter: anything\n",
		"rtc: bad\nbrowser_width: [wrong]\ncode: {}\nexec_policy: wrong\n",
		"[completely broken yaml\n",
		"code: '   '\n",
	} {
		t.Run(fmt.Sprintf("config%d", index), func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("HOME", dir)
			t.Setenv("XDG_CONFIG_HOME", dir)
			t.Setenv("APPDATA", dir)
			t.Setenv("AIC_TEST_CONFIG_STARTUP", "1")
			portFile := filepath.Join(dir, "port.json")
			t.Setenv("AIC_PORT_FILE", portFile)
			p, err := cfg.Path()
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			exe, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, exe, "-test.run=^TestInvalidConfigStillStartsAndCanBeRepaired$")
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
			var info struct {
				Port int    `json:"port"`
				Code string `json:"code"`
			}
			for {
				data, err := os.ReadFile(portFile)
				if err == nil && json.Unmarshal(data, &info) == nil && info.Port > 0 && info.Code != "" {
					break
				}
				select {
				case <-ctx.Done():
					t.Fatal("invalid config blocked the backend handshake")
				case <-time.After(10 * time.Millisecond):
				}
			}
			client := &http.Client{Timeout: time.Second}
			base := fmt.Sprintf("http://127.0.0.1:%d", info.Port)
			// 真实 CLI 进程既能读配置，也能通过设置 API 保存修复。
			for _, call := range []struct{ method, path, body string }{
				{"GET", "/settings", ""},
				{"GET", "/api/get_config", ""},
				{"POST", "/api/set_config", `{"host":"http://localhost:4000","home_path":"/agents","exec_policy":"deny","exec_allow":[],"exec_deny":[],"fs_policy":"deny","fs_allow":[],"fs_deny":[],"net_policy":"deny","net_allow":[],"net_deny":[],"ssh_policy":"deny","ssh_allow":[],"ssh_deny":[]}`},
			} {
				req, err := http.NewRequest(call.method, base+call.path, strings.NewReader(call.body))
				if err != nil {
					t.Fatal(err)
				}
				req.Header.Set("x-aic-code", info.Code)
				req.Header.Set("Content-Type", "application/json")
				if call.path == "/settings" {
					req.Header.Set("Accept", "text/html")
				}
				resp, err := client.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				resp.Body.Close()
				if resp.StatusCode != http.StatusOK {
					t.Fatalf("configuration recovery %s: %d", call.path, resp.StatusCode)
				}
			}
			repaired, err := cfg.LoadFile()
			if err != nil || repaired.Host != "http://localhost:4000" || repaired.HomePath != "/agents" {
				t.Fatalf("configuration repair not persisted: %v", err)
			}
		})
	}
}
