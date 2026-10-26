package host

// Native capability checks belong to the vbox-backed command that needs them,
// not to configuration compilation. These tests pin the platform only for
// diagnostics; they never start a process.

import (
	"strings"
	"testing"

	"github.com/veypi/aic-pod/cfg"
	"github.com/veypi/vbox"
)

func pinNativeNetPlatform(t *testing.T, platform string) {
	t.Helper()
	saved := nativeNetPlatform
	nativeNetPlatform = platform
	t.Cleanup(func() { nativeNetPlatform = saved })
}

func TestCompileAuthDoesNotCheckNativeCapabilities(t *testing.T) {
	for _, platform := range []string{"linux", "windows", "darwin"} {
		t.Run(platform, func(t *testing.T) {
			pinNativeNetPlatform(t, platform)
			base, err := compileAuth(cfg.AuthFrom(&cfg.Options{
				NetPolicy: cfg.PolicyDeny,
				NetRules:  []string{"deny:169.254.169.254:*", "allow:127.0.0.1:8080", "deny:localhost:8080"},
			}))
			if err != nil {
				t.Fatalf("native capability leaked into config compilation: %v", err)
			}
			if len(base.netRules) != 3 {
				t.Fatalf("net rules were filtered: %+v", base.netRules)
			}
		})
	}
}

func TestLegacyExecutionLayerPrefixIsConfigError(t *testing.T) {
	base, err := compileAuth(cfg.AuthFrom(&cfg.Options{NetRules: []string{"tool:deny:169.254.169.254:*"}}))
	if err == nil || base.errs["net"] == nil {
		t.Fatalf("legacy prefix must fail only the net domain: base=%+v err=%v", base, err)
	}
	if base.errs["exec"] != nil || base.errs["fs"] != nil || base.errs["ssh"] != nil {
		t.Fatalf("net syntax error contaminated other domains: %+v", base.errs)
	}
}

func TestInvalidUpstreamProxyFailsOnlyNet(t *testing.T) {
	base, err := compileAuth(cfg.AuthFrom(&cfg.Options{NetUpstreamProxy: "ftp://127.0.0.1:21"}))
	if err == nil || base.errs["net"] == nil {
		t.Fatalf("invalid upstream proxy must fail compilation: base=%+v err=%v", base, err)
	}
	field, ok := base.errs["net"].(*cfg.AuthFieldError)
	if !ok || field.Field != "net_upstream_proxy" {
		t.Fatalf("net error=%T %+v, want net_upstream_proxy field error", base.errs["net"], base.errs["net"])
	}
	if base.errs["exec"] != nil || base.errs["fs"] != nil || base.errs["ssh"] != nil {
		t.Fatalf("proxy error contaminated other domains: %+v", base.errs)
	}
}

func TestNativePolicyUsesCompleteNetSnapshot(t *testing.T) {
	pinNativeNetPlatform(t, "linux")
	withGlobal(t, func(o *cfg.Options) {
		o.NetRules = []string{"deny:169.254.169.254:*", "allow:example.com:443"}
		o.NetUpstreamProxy = "http://127.0.0.1:7897"
	})
	p := newTestPerms(t, "")
	snapshot := p.netSnapshot("s1")
	if snapshot.Match("169.254.169.254:443") {
		t.Fatal("configured deny disappeared from the complete snapshot")
	}
	if !snapshot.Match("example.com:443") {
		t.Fatal("configured allow missing from the complete snapshot")
	}
	if snapshot.UpstreamProxy != "http://127.0.0.1:7897" {
		t.Fatalf("upstream proxy missing from net snapshot: %q", snapshot.UpstreamProxy)
	}
	if _, err := vbox.CheckNetPolicy(nativeNetPlatform, snapshot); !vbox.IsUnsupported(err) {
		t.Fatalf("native capability error = %v, want UnsupportedError at the consumer", err)
	}
}

func TestGrantNetNativeNote(t *testing.T) {
	pinNativeNetPlatform(t, "darwin")
	withGlobal(t, func(o *cfg.Options) { o.NetPolicy = cfg.PolicyDeny })
	c := &Client{perms: newTestPerms(t, "")}
	msg, err := c.grantTarget("s1", "net", "example.com:443", false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(msg, "未逐条落地") {
		t.Fatalf("darwin non-loopback grant now lands in the egress proxy, got %q", msg)
	}
	if !c.perms.netSnapshot("s1").Match("example.com:443") {
		t.Fatal("grant not effective in the Pod net domain")
	}
	msg, err = c.grantTarget("s1", "net", "localhost:8080", false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(msg, "未逐条落地") {
		t.Fatalf("loopback grant lands on darwin, want no note, got %q", msg)
	}
	msg, err = c.grantTarget("s1", "ssh", "10.0.0.2:22", false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(msg, "note:") || strings.Contains(msg, "warn:") {
		t.Fatalf("ssh grant must not carry native net diagnostics, got %q", msg)
	}
}

func TestGrantStatusShowsKernelLanding(t *testing.T) {
	pinNativeNetPlatform(t, "darwin")
	withGlobal(t, func(o *cfg.Options) { o.NetPolicy = cfg.PolicyDeny })
	p := newTestPerms(t, "")
	status := p.grantStatus("s1")
	if !strings.Contains(status, "allow:localhost:* [kernel") {
		t.Fatalf("kernel landing not annotated: %s", status)
	}
}

func TestGrantStatusNetLandingNotes(t *testing.T) {
	pinNativeNetPlatform(t, "linux")
	withGlobal(t, func(o *cfg.Options) {
		o.NetPolicy = cfg.PolicyDeny
		o.NetRules = []string{"allow:example.com:443"}
	})
	p := newTestPerms(t, "")
	status := p.grantStatus("s1")
	if !strings.Contains(status, "allow:localhost:* [absorbed") {
		t.Fatalf("absorbed rows not annotated: %s", status)
	}
	if !strings.Contains(status, "level + original order") {
		t.Fatalf("status does not describe current priority: %s", status)
	}
}
