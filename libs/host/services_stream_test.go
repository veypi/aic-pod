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

// 内建集不再有 browser 包：官方 browser 改走统一 commands 机制（技能包 +
// 根 manifest.json + 设备侧字节流）。本测试用合成清单验证「清单声明流通道」
// 这层机制本身：路由按清单走、所有者覆盖不继承、禁用即摘除。
func TestManifestStreamChannelFollowsManifestAndDropsOnOverride(t *testing.T) {
	c, _ := testClient(t)
	provider := &manifestStreamProvider{channel: "aic-provider-default"}
	manifest := &packManifest{Alias: "fixture", Stream: &packStream{Channel: "aic-fixture-manifest"}}
	if err := c.registerServiceProvider("fixture", provider, nil, manifest); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.streamResolver(manifest.Stream.Channel); !ok {
		t.Fatal("manifest channel was not routed")
	}
	if _, ok := c.streamResolver(provider.channel); ok {
		t.Fatal("provider default channel was advertised instead of the manifest one")
	}
	// 所有者覆盖是纯命令定义（不带清单），不继承清单声明的流。
	c.unregisterService("fixture")
	if err := c.registerService("fixture", mcpx.Config{Command: "custom-fixture"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.streamResolver(manifest.Stream.Channel); ok {
		t.Fatal("owner-overridden service inherited the manifest stream")
	}
	// 恢复带清单的定义 → 路由回来。
	c.unregisterService("fixture")
	if err := c.registerServiceProvider("fixture", provider, nil, manifest); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.streamResolver(manifest.Stream.Channel); !ok {
		t.Fatal("restored manifest definition did not restore its route")
	}
	c.syncServiceRegistrations(map[string]mcpx.Config{"fixture": {Disabled: true}})
	if _, ok := c.streamResolver(manifest.Stream.Channel); ok {
		t.Fatal("disabled service retained its stream route")
	}
}
