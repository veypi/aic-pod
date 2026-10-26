package host

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/veypi/aic-pod/libs/mcpx"
	"github.com/veypi/aic-pod/libs/vtools"
	"github.com/veypi/aic-pod/protocol"
)

type manifestStreamProvider struct {
	channel string
	calls   atomic.Int32
}

func (p *manifestStreamProvider) Tools(context.Context) ([]vtools.Tool, error) { return nil, nil }
func (p *manifestStreamProvider) Call(context.Context, string, map[string]any) (any, error) {
	return nil, nil
}
func (p *manifestStreamProvider) StreamChannel() string { return p.channel }
func (p *manifestStreamProvider) Stream(context.Context, protocol.Caller, <-chan []byte, func([]byte) error) error {
	p.calls.Add(1)
	return nil
}

type manifestPlainProvider struct{}

func (manifestPlainProvider) Tools(context.Context) ([]vtools.Tool, error) { return nil, nil }
func (manifestPlainProvider) Call(context.Context, string, map[string]any) (any, error) {
	return nil, nil
}

func TestManifestStreamReachesRTCStreamResolver(t *testing.T) {
	c, _ := testClient(t)
	p := &manifestStreamProvider{channel: "aic-provider-default"}
	m := &packManifest{Alias: "fixture", Stream: &packStream{Channel: "aic-from-manifest"}}
	if err := c.registerServiceProvider("fixture", p, nil, m); err != nil {
		t.Fatal(err)
	}
	streams := c.newRTCConfig(make([]byte, 32)).Streams
	if _, ok := streams(p.channel); ok {
		t.Fatal("RTC exposed provider default instead of manifest route")
	}
	relay, ok := streams(m.Stream.Channel)
	if !ok {
		t.Fatal("RTC resolver cannot find manifest stream handler")
	}
	c.perms.grantCmd("s1", "mcp.fixture")
	if err := relay(context.Background(), protocol.Caller{Origin: "s1"}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if got := p.calls.Load(); got != 1 {
		t.Fatalf("provider Stream calls = %d, want 1", got)
	}
}

func TestManifestStreamRequiresRealStreamerAndValidChannel(t *testing.T) {
	c, _ := testClient(t)
	m := &packManifest{Alias: "plain", Stream: &packStream{Channel: "aic-plain"}}
	err := c.registerServiceProvider("plain", manifestPlainProvider{}, nil, m)
	if err == nil || !strings.Contains(err.Error(), "does not implement vtools.Streamer") {
		t.Fatalf("stream declaration without Streamer: %v", err)
	}
	if c.serviceRegistry().Has("plain") {
		t.Fatal("unsupported stream declaration left a service registered")
	}
	if _, ok := c.streamResolver(m.Stream.Channel); ok {
		t.Fatal("unsupported provider was advertised as a stream")
	}
	for _, channel := range []string{"", protocol.RtcChannel, " aic-invalid"} {
		m.Stream.Channel = channel
		if err := c.registerServiceProvider("plain", &manifestStreamProvider{}, nil, m); err == nil {
			t.Fatalf("invalid manifest channel accepted: %q", channel)
		}
		if c.serviceRegistry().Has("plain") {
			t.Fatalf("invalid channel left a service registered: %q", channel)
		}
	}
}

func TestManifestStreamCollisionAndRemoval(t *testing.T) {
	c, _ := testClient(t)
	m := &packManifest{Stream: &packStream{Channel: "aic-shared-manifest"}}
	if err := c.registerServiceProvider("first", &manifestStreamProvider{}, nil, m); err != nil {
		t.Fatal(err)
	}
	if err := c.registerServiceProvider("second", &manifestStreamProvider{}, nil, m); err == nil {
		t.Fatal("duplicate manifest channel accepted")
	}
	if c.serviceRegistry().Has("second") {
		t.Fatal("conflicting service was left registered")
	}
	if _, ok := c.streamResolver(m.Stream.Channel); !ok {
		t.Fatal("conflict removed the original route")
	}
	c.unregisterService("first")
	if _, ok := c.streamResolver(m.Stream.Channel); ok {
		t.Fatal("route survived service removal")
	}
	if err := c.registerServiceProvider("second", &manifestStreamProvider{}, nil, m); err != nil {
		t.Fatalf("channel was not reusable after removal: %v", err)
	}
	c.unregisterService("second")
	if _, ok := c.streamResolver(m.Stream.Channel); ok {
		t.Fatal("replacement route survived service removal")
	}
}

func TestBrowserManifestChannelAndOwnerOverride(t *testing.T) {
	packs, err := builtinPacks()
	if err != nil {
		t.Fatal(err)
	}
	original := packs["browser"]
	if original == nil || original.Stream == nil {
		t.Fatal("browser pack must declare its stream channel")
	}
	manifest := *original
	manifest.Stream = &packStream{Channel: "aic-browser-test-manifest"}
	c, _ := testClient(t)
	if _, ok := c.streamResolver(original.Stream.Channel); !ok {
		t.Fatal("default service sync did not consume the embedded browser manifest")
	}
	c.svc.mu.RLock()
	config := *c.svc.browser
	c.svc.mu.RUnlock()
	c.unregisterService("browser")
	if err := c.registerServiceWithManifest("browser", config, &manifest); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.streamResolver(manifest.Stream.Channel); !ok {
		t.Fatal("browser did not use the channel from its manifest")
	}
	if _, ok := c.streamResolver(original.Stream.Channel); ok {
		t.Fatal("browser retained a hard-coded/default channel alongside manifest")
	}
	// Owner override is still MCP-shaped, but must not inherit the built-in stream.
	if err := c.registerService("browser", mcpx.Config{Command: "custom-browser"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.streamResolver(manifest.Stream.Channel); ok {
		t.Fatal("owner-overridden browser inherited built-in stream support")
	}
	// Restoring the built-in definition reattaches the same real stream adapter.
	if err := c.registerServiceWithManifest("browser", config, &manifest); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.streamResolver(manifest.Stream.Channel); !ok {
		t.Fatal("restored built-in browser did not restore its manifest route")
	}
	c.syncServiceRegistrations(map[string]mcpx.Config{"browser": {Disabled: true}})
	if _, ok := c.streamResolver(manifest.Stream.Channel); ok {
		t.Fatal("disabled browser retained its stream route")
	}
}
