package host

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/veypi/aic-pod/protocol"
	"github.com/veypi/vsh/commands"
)

func TestFSAndVSHRGShareResidentIndex(t *testing.T) {
	c := newConfinedTestClient(t)
	root := c.options().WorkDir
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("needle\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	canonical := hostCanonical(root)
	deadline := time.Now().Add(5 * time.Second)
	for {
		paths, ok := c.index.Candidates(context.Background(), commands.FileIndexQuery{Root: canonical, FilesOnly: true})
		if ok && slices.Contains(paths, canonical+"/a.txt") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("resident index did not become ready")
		}
		time.Sleep(20 * time.Millisecond)
	}

	result := runManagedScript(t, c, "rg needle .", "")
	if result.ExitCode != 0 || result.Stdout != "./a.txt:needle\n" {
		t.Fatalf("vsh rg = %+v", result)
	}
	args, err := json.Marshal(map[string]any{"path": root, "pattern": "needle"})
	if err != nil {
		t.Fatal(err)
	}
	value, err := c.handleFS(context.Background(), protocol.Caller{Subject: "owner", Origin: "s1"}, &protocol.FSInvocation{
		Method: "text", Action: "rg", Args: args,
	})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), "needle") || !strings.Contains(string(encoded), "a.txt") {
		t.Fatalf("fs rg = %s", encoded)
	}
}
