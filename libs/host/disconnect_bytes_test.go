package host

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/veypi/aic-pod/cfg"
	"github.com/veypi/aic-pod/libs/hostfs"
	"github.com/veypi/aic-pod/protocol"
)

func uploadCallerSource(t *testing.T, c *Client, caller protocol.Caller, data []byte) hostfs.ByteSource {
	t.Helper()
	size := int64(len(data))
	src, err := c.bytes.UploadFor(context.Background(), hostfs.Owner(caller), hostfs.Creator(caller), bytes.NewReader(data), &size, "", "")
	if err != nil {
		t.Fatal(err)
	}
	return src
}

func TestDisconnectClosesOnlyCreatorByteSources(t *testing.T) {
	withGlobal(t, nil)
	c, _ := testClient(t)
	defer c.Close()
	rtcA := testCaller()
	rtcB := rtcA
	rtcB.ConnectionID = "rtc-b"
	nats := rtcA
	nats.ConnectionID = "nats:" + rtcA.Subject
	owner := hostfs.Owner(rtcA)
	if owner != hostfs.Owner(rtcB) || owner != hostfs.Owner(nats) {
		t.Fatal("test premise changed: callers no longer share the session owner")
	}
	srcA := uploadCallerSource(t, c, rtcA, []byte("a"))
	srcB := uploadCallerSource(t, c, rtcB, []byte("b"))
	srcNATS := uploadCallerSource(t, c, nats, []byte("n"))

	c.DisconnectTools(rtcA)
	if _, err := c.bytes.Describe(owner, srcA.Ref); err == nil {
		t.Fatal("source created by the disconnected RTC was not reclaimed")
	}
	for _, src := range []hostfs.ByteSource{srcB, srcNATS} {
		if _, err := c.bytes.Describe(owner, src.Ref); err != nil {
			t.Fatalf("unrelated source was reclaimed: %v", err)
		}
	}

	c.DisconnectTools(rtcB)
	if _, err := c.bytes.Describe(owner, srcB.Ref); err == nil {
		t.Fatal("source created by the second disconnected RTC was not reclaimed")
	}
	if _, err := c.bytes.Describe(owner, srcNATS.Ref); err != nil {
		t.Fatalf("NATS source was reclaimed by an RTC disconnect: %v", err)
	}
}

func TestDisconnectFreesSourceQuotaForTextEdit(t *testing.T) {
	withGlobal(t, func(o *cfg.Options) { o.HostsSources = 1 })
	c, _ := testClient(t)
	defer c.Close()
	caller := testCaller()

	// Simulate repeated page reconnects with one leaked source per connection.
	// Every disconnect must release the sole slot before the next creation.
	for i := 0; i < 3; i++ {
		current := caller
		current.ConnectionID = "rtc-reconnect-" + string(rune('a'+i))
		src := uploadCallerSource(t, c, current, []byte("pending"))
		if src.Ref.ID == "" {
			t.Fatal("empty source ID")
		}
		c.DisconnectTools(current)
	}

	path := filepath.Join(c.options().WorkDir, "edited.txt")
	response := callTool(t, c, context.Background(), caller, fsRequest("text.write", map[string]any{
		"path": filepath.ToSlash(path), "content": "changed",
	}))
	if response.Error != nil {
		t.Fatalf("text edit failed after disconnect cleanup: %+v", response)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "changed" {
		t.Fatalf("edited content=%q, want changed", data)
	}
}
