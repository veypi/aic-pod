package skillrun

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/veypi/vsh/commands"
)

func TestProcessProviderEnvironment(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses /bin/sh as a process provider")
	}
	t.Setenv("AIC_INHERITED_PROBE", "must-not-leak")
	r, _ := newTestRegistry(t, true)
	dir := t.TempDir()
	provider := Manifest{Kind: KindProcess, Entry: "probe"}
	pkg := &Package{Name: "envprobe", Dir: dir, Manifest: &provider}
	if err := os.WriteFile(filepath.Join(dir, provider.Entry), []byte("#!/bin/sh\nprintf '%s|%s|%s' \"$DEMO\" \"$EMPTY\" \"$AIC_INHERITED_PROBE\"\n"), 0755); err != nil {
		t.Fatal(err)
	}
	var out, errs strings.Builder
	err := r.runProcess(context.Background(), &commands.Invocation{
		Cwd: dir, Env: map[string]string{"DEMO": "spaces = x", "EMPTY": ""}, Stdout: &out, Stderr: &errs,
	}, pkg)
	if err != nil || out.String() != "spaces = x||" || errs.Len() != 0 {
		t.Fatalf("stdout=%q stderr=%q err=%v", out.String(), errs.String(), err)
	}
}
