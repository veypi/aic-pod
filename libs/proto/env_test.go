package proto

import (
	"os"
	"reflect"
	"runtime"
	"testing"
)

func TestHostEnvironmentPaths(t *testing.T) {
	path, home := "/usr/bin:/bin", "/home/agent"
	if runtime.GOOS == "windows" {
		path, home = `C:\Windows\System32;C:\Tools`, `C:\Users\agent`
	}
	env := map[string]string{
		"PATH": NormalizeHostPathList(path), "HOME": NormalizeHostPath(home),
		"EMPTY": "", "DEMO": "value with spaces=a:b", "TMPDIR": "/tmp",
	}
	tmp := "/tmp"
	if runtime.GOOS == "windows" {
		tmp = os.TempDir()
	}
	want := []string{"DEMO=value with spaces=a:b", "EMPTY=", "HOME=" + home, "PATH=" + path, "TMPDIR=" + tmp}
	wantMap := map[string]string{"DEMO": "value with spaces=a:b", "EMPTY": "", "HOME": home, "PATH": path, "TMPDIR": tmp}
	if got := HostEnvMapToOS(env); !reflect.DeepEqual(got, wantMap) {
		t.Fatalf("service environment=%q, want %q", got, wantMap)
	}
	if got := HostEnvToOS(env); !reflect.DeepEqual(got, want) {
		t.Fatalf("environment=%q, want %q", got, want)
	}
	if env["TMPDIR"] != "/tmp" {
		t.Fatal("conversion mutated invocation")
	}
	seed := HostEnvFromOS([]string{"PATH=" + path, "HOME=" + home, "EMPTY=", "DEMO=value with spaces=a:b"})
	delete(env, "TMPDIR")
	if !reflect.DeepEqual(seed, env) {
		t.Fatalf("shell environment=%q, want %q", seed, env)
	}
}
