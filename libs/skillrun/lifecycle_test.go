package skillrun

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/veypi/vsh/commands"
)

func TestActiveProcessBlocksReplaceAndRemove(t *testing.T) {
	r, reg := newTestRegistry(t, true)
	src := buildHelloPkg(t)
	pkg, err := installTestPackage(t, r, src)
	if err != nil {
		t.Fatal(err)
	}
	cmd, _ := reg.Lookup("hello")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- cmd.Run(ctx, commands.NewInvocation(&commands.InvocationOptions{Args: []string{"sleep", "30"}, Cwd: src}))
	}()
	deadline := time.Now().Add(2 * time.Second)
	for {
		pkg.mu.Lock()
		active := pkg.active
		pkg.mu.Unlock()
		if active > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("process did not start")
		}
		time.Sleep(time.Millisecond)
	}
	if _, err := installTestPackage(t, r, src); err == nil || !strings.Contains(err.Error(), "busy") {
		t.Fatalf("update err=%v", err)
	}
	if err := r.Uninstall("hello"); err == nil || !strings.Contains(err.Error(), "busy") {
		t.Fatalf("remove err=%v", err)
	}
	if r.Get("hello") != pkg {
		t.Fatal("active package replaced")
	}
	if err := r.SetDisabled("hello", true); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Run(context.Background(), commands.NewInvocation(nil)); err == nil {
		t.Fatal("disabled package executed")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("process did not stop")
	}
}

func TestRetainedRootUsesUpdatedPackage(t *testing.T) {
	r, reg := newTestRegistry(t, true)
	src := buildHelloPkg(t)
	if _, err := installTestPackage(t, r, src); err != nil {
		t.Fatal(err)
	}
	cmd, _ := reg.Lookup("hello")
	if err := os.WriteFile(filepath.Join(src, "cli", "manifest.json"), []byte(`{"kind":"process","entry":"cli/bin/hello-process","args":["argv","updated"]}`), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := installTestPackage(t, r, src); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := cmd.Run(context.Background(), commands.NewInvocation(&commands.InvocationOptions{Cwd: src, Stdout: &out})); err != nil {
		t.Fatal(err)
	}
	if out.String() != "updated\n" {
		t.Fatalf("retained root=%q", out.String())
	}
}

func TestServiceConcurrentReadinessAndDisableWait(t *testing.T) {
	r, _ := newTestRegistry(t, true)
	p, err := installTestPackage(t, r, buildHelloServicePkg(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _ = r.ensureService(ctx, p)
	const callers = 12
	instances := make(chan *serviceInst, callers)
	errs := make(chan error, callers)
	var wg sync.WaitGroup
	for range callers {
		wg.Go(func() { inst, err := r.ensureService(context.Background(), p); instances <- inst; errs <- err })
	}
	wg.Wait()
	close(instances)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	inst := packageService(r, "hello")
	for got := range instances {
		if got != inst {
			t.Fatal("concurrent callers started different services")
		}
	}
	if err := r.SetDisabled("hello", true); err != nil {
		t.Fatal(err)
	}
	select {
	case <-inst.done:
	default:
		t.Fatal("disable returned before process exit")
	}
	if _, err := r.ensureService(context.Background(), p); err == nil {
		t.Fatal("disabled service restarted")
	}
}

func TestRecoveryAndResourceNameIsolation(t *testing.T) {
	r, reg := newTestRegistry(t, true)
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "README.md"), []byte("resource"), 0644); err != nil {
		t.Fatal(err)
	}
	builtin := commands.DefineCommand("echo", func(context.Context, *commands.Invocation) error { return nil })
	if err := reg.Register(builtin); err != nil {
		t.Fatal(err)
	}
	meta := &FetchMeta{Name: "echo", Kind: "public", ID: "resource"}
	pkg, err := r.InstallZip(context.Background(), zipDir(t, src), meta)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Uninstall("echo"); err != nil {
		t.Fatal(err)
	}
	if got, _ := reg.Lookup("echo"); got != builtin {
		t.Fatal("resource uninstall removed builtin")
	}
	pkg, err = r.InstallZip(context.Background(), zipDir(t, src), meta)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(pkg.Dir, pkg.Dir+".old"); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(pkg.Dir+".next", 0700); err != nil {
		t.Fatal(err)
	}
	fresh, err := New(r.deps)
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	fresh.Rescan()
	if got := fresh.Get("echo"); got == nil || got.Record().ID != "resource" {
		t.Fatal("old package not recovered")
	}
	for _, suffix := range []string{".old", ".next"} {
		if _, err := os.Stat(pkg.Dir + suffix); !os.IsNotExist(err) {
			t.Fatalf("leftover %s: %v", suffix, err)
		}
	}
}

func TestRecoveryPreservesInvalidFinalAndBackup(t *testing.T) {
	r, _ := newTestRegistry(t, true)
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "README.md"), []byte("old resource"), 0644); err != nil {
		t.Fatal(err)
	}
	pkg, err := r.InstallZip(context.Background(), zipDir(t, src), &FetchMeta{Name: "resource", Kind: "public", ID: "resource"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(pkg.Dir, pkg.Dir+".old"); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{pkg.Dir, pkg.Dir + ".next"} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "partial"), []byte("unfinished update"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.recoverPackage("resource"); err == nil {
		t.Fatal("corrupt final directory must require explicit repair")
	}
	for _, suffix := range []string{"", ".next"} {
		if got, err := os.ReadFile(filepath.Join(pkg.Dir+suffix, "partial")); err != nil || string(got) != "unfinished update" {
			t.Fatalf("recovery changed %q: %q, %v", suffix, got, err)
		}
	}
	if got, err := os.ReadFile(filepath.Join(pkg.Dir+".old", "README.md")); err != nil || string(got) != "old resource" {
		t.Fatalf("backup changed: %q, %v", got, err)
	}
}
