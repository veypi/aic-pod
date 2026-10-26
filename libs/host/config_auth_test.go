package host

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/veypi/aic-pod/cfg"
	"github.com/veypi/aic-pod/protocol"
)

func TestAuthErrorsAreDomainLocal(t *testing.T) {
	saved := cfg.Global
	t.Cleanup(func() { cfg.Global = saved })
	cases := []struct {
		domain  string
		corrupt func(*cfg.Options)
	}{
		{"exec", func(o *cfg.Options) { o.ExecPolicy = "dney" }},
		{"exec", func(o *cfg.Options) { o.ExecRules = []string{"fixture", "bad rule"} }},
		{"fs", func(o *cfg.Options) { o.FsRules = []string{"rw:/private", "rw:**"} }},
		{"net", func(o *cfg.Options) { o.NetRules = []string{"allow:example.com", "allow:*:443"} }},
		{"ssh", func(o *cfg.Options) { o.SshRules = []string{"allow:*:22"} }},
	}
	for _, tc := range cases {
		t.Run(tc.domain, func(t *testing.T) {
			cfg.Global = cfg.NewOptions()
			cfg.Global.FsPolicy = cfg.PolicyOpen
			cfg.Global.SshPolicy = cfg.PolicyOpen
			tc.corrupt(cfg.Global)
			cfg.Global.Normalize()
			c, _ := testClient(t)
			defer c.Close()
			for _, domain := range []string{"exec", "fs", "net", "ssh"} {
				err := c.perms.err(domain)
				if domain == tc.domain && err == nil {
					t.Fatalf("%s error not recorded", domain)
				}
				if domain != tc.domain && err != nil {
					t.Fatalf("%s error contaminated %s: %v", tc.domain, domain, err)
				}
			}

			path := filepath.Join(c.options().WorkDir, "keep.txt")
			if err := os.WriteFile(path, []byte("original"), 0o600); err != nil {
				t.Fatal(err)
			}
			execResponses := []protocol.Response{
				callTool(t, c, context.Background(), testCaller(), execRequest("printf ok", 30000)),
				signedCall(t, c, execRequest("printf ok", 30000), false, "s1", ""),
			}
			for _, response := range execResponses {
				if response.Error != nil {
					t.Fatalf("exec became a protocol failure: %+v", response)
				}
				out := decoded[protocol.Output](t, response.Result)
				want := "0"
				if tc.domain == "exec" {
					want = "126"
				}
				if out.Attrs["exit_code"] != want {
					t.Fatalf("%s: exec exit=%q want %s (%+v)", tc.domain, out.Attrs["exit_code"], want, out)
				}
			}

			fsResponses := []protocol.Response{
				callTool(t, c, context.Background(), testCaller(), fsRequest("text.write", map[string]any{"path": filepath.ToSlash(path), "content": "changed"})),
				signedCall(t, c, fsRequest("text.write", map[string]any{"path": filepath.ToSlash(path), "content": "changed"}), false, "s1", ""),
			}
			for _, response := range fsResponses {
				if tc.domain == "fs" {
					if response.Error == nil || response.Error.Code != "permission_denied" {
						t.Fatalf("FS error did not reject the FS request: %+v", response)
					}
				} else if response.Error != nil {
					t.Fatalf("%s blocked an unrelated FS request: %+v", tc.domain, response)
				}
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			wantContent := "changed"
			if tc.domain == "fs" {
				wantContent = "original"
			}
			if string(data) != wantContent {
				t.Fatalf("%s: file content=%q want %q", tc.domain, data, wantContent)
			}

			netAllowed := c.perms.netSnapshot("s1").Match("example.com:443")
			if tc.domain == "net" && netAllowed {
				t.Fatal("invalid net snapshot did not fail closed")
			}
			if tc.domain != "net" && !netAllowed {
				t.Fatalf("%s blocked the net domain", tc.domain)
			}
			sshAllowed := c.perms.sshAllowed("s1", "example.com:22")
			if tc.domain == "ssh" && sshAllowed {
				t.Fatal("invalid ssh snapshot did not fail closed")
			}
			if tc.domain != "ssh" && !sshAllowed {
				t.Fatalf("%s blocked the ssh domain", tc.domain)
			}
		})
	}
}
