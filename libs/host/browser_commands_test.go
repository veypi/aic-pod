package host

import (
	"context"
	"testing"
	"time"

	"github.com/veypi/aic-pod/libs/hostcmd"
	"github.com/veypi/aic-pod/libs/proto"
)

func TestBrowserProviderStartupReconcile(t *testing.T) {
	providersMu.Lock()
	previous, exists := providers["browser"]
	delete(providers, "browser")
	providersMu.Unlock()
	t.Cleanup(func() {
		providersMu.Lock()
		defer providersMu.Unlock()
		if exists {
			providers["browser"] = previous
		} else {
			delete(providers, "browser")
		}
	})
	c := New(Options{Key: "host_1.1.secret.owner", WorkDir: t.TempDir()})
	service, err := c.NewCommandService(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close(context.Background())
	c.commands = service
	// The provider arrives after Runtime creation, before runtime.Start publishes
	// rtClient. The final startup reconciliation must populate both catalogs.
	providersMu.Lock()
	providers["browser"] = Provider{Decl: proto.CommandDecl{Name: "browser"}, Direct: &ShellChannel{Addr: "127.0.0.1:1", Token: "fixture"}}
	providersMu.Unlock()
	c.syncProviders()
	_, catalog, err := service.Runtime.Catalog(hostcmd.Caller{Subject: "owner", ConnectionID: "fixture", ExpiresAt: time.Now().Add(time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	for _, command := range catalog {
		if command.Name == "browser" {
			if command.Contract != "browser/1" || command.Methods["view"].Mode != "live" || command.Methods["navigate"].Mode != "" {
				t.Fatal("invalid browser command contract")
			}
			return
		}
	}
	t.Fatal("late browser registration missing from direct command catalog")
}
