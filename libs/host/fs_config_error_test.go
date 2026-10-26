package host

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/veypi/aic-pod/cfg"
	"github.com/veypi/aic-pod/libs/execution"
	"github.com/veypi/vbox"
)

func TestInvalidFSConfigHostFSReadFailsClosed(t *testing.T) {
	withGlobal(t, func(o *cfg.Options) { o.FsRules = []string{"rw:**"} })
	p := newTestPerms(t, "")
	workdir := t.TempDir()
	name := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(name, []byte("probe"), 0o600); err != nil {
		t.Fatal(err)
	}
	fsys, err := execution.NewHostFS(execution.HostFSConfig{
		Backing:    OSVFS{},
		Rules:      func() vbox.FSRuleSet { return p.fsSnapshot("s1") },
		WorkingDir: filepath.ToSlash(workdir),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := fsys.Chdir(filepath.ToSlash(workdir)); err != nil {
		t.Fatalf("working-directory bootstrap was blocked: %v", err)
	}
	if _, err := fsys.Stat(context.Background(), filepath.ToSlash(name)); err == nil {
		t.Fatal("metadata outside the working directory was allowed")
	}
	if _, err := fsys.ReadDir(context.Background(), filepath.ToSlash(workdir)); err == nil {
		t.Fatal("working-directory listing was allowed during bootstrap exception")
	}
	f, err := fsys.Open(context.Background(), filepath.ToSlash(name))
	if err == nil {
		_ = f.Close()
		t.Fatal("HostFS opened a file while the FS authorization config was invalid")
	}
}

func TestInvalidFSConfigNativeSnapshotCarriesError(t *testing.T) {
	withGlobal(t, func(o *cfg.Options) { o.FsRules = []string{"rw:**"} })
	p := newTestPerms(t, "")
	policy := p.fsSnapshotForNative("s1", t.TempDir(), "cat")
	if policy.Err == nil {
		t.Fatal("native FS snapshot dropped the configuration error")
	}
	if decision := policy.Match("/any/file", vbox.OpRead); decision.Allow {
		t.Fatal("native FS snapshot allowed a read with invalid configuration")
	}
}
