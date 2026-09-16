//go:build darwin

package exec_procs

import (
	"context"
	"github.com/veypi/aic-pod/libs/fsauth"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHostPolicyNativeEnforcement(t *testing.T) {
	if os.Getenv("AIC_SANDBOX_PROBE") != "1" {
		t.Skip("explicit native sandbox probe")
	}
	base := canonicalRoot(t.TempDir())
	rw, ro, out := filepath.Join(base, "rw"), filepath.Join(base, "ro"), filepath.Join(base, "outside")
	for _, dir := range []string{rw, ro, out} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "data"), []byte("original"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	deny := filepath.Join(rw, "blocked")
	if err := os.WriteFile(deny, []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	read := []string{rw + "/**", ro + "/**"}
	for _, r := range fsauth.RuntimeReadRoots() {
		read = append(read, r+"/**")
	}
	run := func(script string) error {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		plan, err := planConfined(confineSpec{level: 9, workdir: rw, extra: []string{rw}, readAllow: read, deny: []string{deny}, netOpen: true, argv: []string{"/bin/sh", "-c", script}})
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.CommandContext(ctx, plan.argv[0], plan.argv[1:]...)
		cmd.Dir = rw
		b, e := cmd.CombinedOutput()
		if e != nil {
			t.Logf("sandbox: %s", b)
		}
		return e
	}
	q := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
	if err := run("/bin/cat " + q(filepath.Join(ro, "data"))); err != nil {
		t.Fatalf("read-only read failed: %v", err)
	}
	if err := run("echo changed > " + q(filepath.Join(rw, "data"))); err != nil {
		t.Fatalf("allowed write failed: %v", err)
	}
	for _, script := range []string{"echo changed > " + q(filepath.Join(ro, "data")), "/bin/cat " + q(filepath.Join(out, "data")), "/bin/cat " + q(deny), "echo changed > " + q(deny)} {
		if err := run(script); err == nil {
			t.Fatalf("forbidden effect succeeded: %s", script)
		}
	}
	b, _ := os.ReadFile(filepath.Join(ro, "data"))
	if string(b) != "original" {
		t.Fatal("read-only file changed")
	}
	b, _ = os.ReadFile(deny)
	if string(b) != "secret" {
		t.Fatal("denied file changed")
	}
}
