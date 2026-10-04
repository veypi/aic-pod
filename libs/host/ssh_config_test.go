package host

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeSSHTestConfig(t *testing.T, home, text string) string {
	t.Helper()
	p := filepath.Join(home, ".ssh", "config")
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestManagedSSHStaticConfig(t *testing.T) {
	home := t.TempDir()
	writeSSHTestConfig(t, home, "Host prod\n HostName=EXAMPLE.COM.\n User deploy\n Port 2222\n IdentityFile \"~/.ssh/key with spaces\"\n IdentitiesOnly yes\nHost * !excluded\n User fallback\n Port 22\n IdentityFile ~/.ssh/second\n")
	target, err := resolveSSHTarget("prod", home, 0)
	if err != nil || target.Host != "example.com" || target.User != "deploy" || target.Port != 2222 || !target.IdentitiesOnly || len(target.Identities) != 2 || target.Identities[0] != filepath.Join(home, ".ssh", "key with spaces") {
		t.Fatalf("target=%+v error=%v", target, err)
	}
	target, err = resolveSSHTarget("other@prod:2200", home, 0)
	if err != nil || target.User != "other" || target.Port != 2200 {
		t.Fatalf("override=%+v %v", target, err)
	}
	if _, err = resolveSSHTarget("prod:22", home, 23); err == nil {
		t.Fatal("conflicting port accepted")
	}
}

func TestManagedSSHConfigNeverExecutesAndFailsClosed(t *testing.T) {
	for _, directive := range []string{"Match exec \"touch marker\"", "ProxyCommand touch marker", "ProxyJump jump", "KnownHostsCommand touch marker", "LocalCommand touch marker", "ControlPath /tmp/control", "Include missing", "Include ../escape", "Include *.conf"} {
		t.Run(directive, func(t *testing.T) {
			home := t.TempDir()
			writeSSHTestConfig(t, home, "Host *\n"+directive+"\n")
			if _, err := resolveSSHTarget("example.com", home, 0); err == nil {
				t.Fatal("unsafe/unsupported config accepted")
			}
			if _, err := os.Stat(filepath.Join(home, "marker")); !os.IsNotExist(err) {
				t.Fatal("unexpected config side effect")
			}
		})
	}
}

func TestManagedSSHIncludeAndDeviceOverride(t *testing.T) {
	home := t.TempDir()
	config := writeSSHTestConfig(t, home, "Include static.conf\n")
	include := filepath.Join(filepath.Dir(config), "static.conf")
	if err := os.WriteFile(include, []byte("Host prod\n HostName 192.0.2.7\n User deploy\n"), 0600); err != nil {
		t.Fatal(err)
	}
	target, err := resolveSSHTarget("prod", home, 0)
	if err != nil || target.Host != "192.0.2.7" {
		t.Fatalf("%+v %v", target, err)
	}
	if err := os.WriteFile(include, []byte("Include config\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = resolveSSHTarget("prod", home, 0); err == nil {
		t.Fatal("include cycle accepted")
	}
	writeSSHTestConfig(t, home, "Match exec \"touch marker\"\n")
	device := filepath.Join(home, ".aic")
	os.MkdirAll(device, 0700)
	if err := os.WriteFile(filepath.Join(device, "ssh_config"), []byte("Host prod\n HostName 192.0.2.8\n"), 0600); err != nil {
		t.Fatal(err)
	}
	target, err = resolveSSHTarget("prod", home, 0)
	if err != nil || target.Host != "192.0.2.8" {
		t.Fatalf("%+v %v", target, err)
	}
}

func TestManagedSSHExternalStaticInclude(t *testing.T) {
	home := t.TempDir()
	orb := filepath.Join(home, ".orbstack", "ssh", "config")
	if err := os.MkdirAll(filepath.Dir(orb), 0700); err != nil {
		t.Fatal(err)
	}
	// Real OrbStack shape: directives unsupported by managed SSH only apply to
	// orb. Reading this file must neither execute its helper nor break pi/win.
	if err := os.WriteFile(orb, []byte("Host orb\n HostName 127.0.0.1\n Port 32222\n User default\n IdentityFile ~/.orbstack/ssh/id_ed25519\n IdentitiesOnly yes\n ProxyCommand touch marker\n ProxyUseFdpass yes\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, include := range []string{"~/.orbstack/ssh/config", "../.orbstack/ssh/config", filepath.ToSlash(orb)} {
		config := writeSSHTestConfig(t, home, "Include \""+include+"\"\nHost pi\n HostName 192.0.2.73\n User pi\nHost win\n HostName 192.0.2.119\n User v\n")
		for alias, host := range map[string]string{"pi": "192.0.2.73", "win": "192.0.2.119"} {
			target, err := resolveSSHTarget(alias, home, 0)
			if err != nil || target.Host != host || len(target.Identities) != 0 {
				t.Fatalf("Include %q, alias %s: %+v %v", include, alias, target, err)
			}
		}
		if _, err := resolveSSHTarget("orb", home, 0); err == nil || !strings.Contains(err.Error(), "ProxyCommand") || !strings.Contains(err.Error(), config+":1:") {
			t.Fatalf("active proxy must fail with source context: %v", err)
		}
	}
	if err := os.WriteFile(orb, []byte("Include config\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveSSHTarget("win", home, 0); err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("external Include cycle accepted: %v", err)
	}
}

func TestManagedSSHIncludeErrorsAndDeviceBoundary(t *testing.T) {
	home := t.TempDir()
	for _, tc := range []struct{ include, reason string }{
		{"~/.missing/config", "no such file"},
		{"~/.ssh/*.conf", "glob"},
		{"${HOME}/.ssh/config", "environment"},
		{"%d/.ssh/config", "token"},
		{"~another/.ssh/config", "only ~/"},
	} {
		config := writeSSHTestConfig(t, home, "# config\nInclude "+tc.include+"\n")
		_, err := resolveSSHTarget("win", home, 0)
		if err == nil || !strings.Contains(err.Error(), config+":2: SSH Include target") || !strings.Contains(err.Error(), tc.include) || strings.Contains(err.Error(), "unsupported static SSH directive Include") {
			t.Fatalf("missing source/target diagnostic: %v", err)
		}
		if tc.reason != "no such file" && !strings.Contains(err.Error(), tc.reason) {
			t.Fatalf("missing reason %q: %v", tc.reason, err)
		}
	}
	device := filepath.Join(home, ".aic")
	if err := os.MkdirAll(filepath.Join(device, "ssh"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(device, "ssh", "win"), []byte("Host win\n HostName 192.0.2.119\n"), 0600); err != nil {
		t.Fatal(err)
	}
	profile := filepath.Join(device, "ssh_config")
	for _, include := range []string{"ssh/win", "~/.aic/ssh/win", "../.ssh/config"} {
		if err := os.WriteFile(profile, []byte("Include "+include+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		target, err := resolveSSHTarget("win", home, 0)
		if include == "../.ssh/config" {
			if err == nil || !strings.Contains(err.Error(), "inside ~/.aic/ssh") {
				t.Fatalf("device boundary not enforced: %v", err)
			}
		} else if err != nil || target.Host != "192.0.2.119" {
			t.Fatalf("device Include %q: %+v %v", include, target, err)
		}
	}
}

func TestManagedSSHRemotePaths(t *testing.T) {
	for _, tc := range []struct{ cwd, name, want string }{
		{"/C:/Users/v", "C:/Users/v/x", "/C:/Users/v/x"},
		{"/C:/Users/v", "/C:/Users/v/x", "/C:/Users/v/x"},
		{"C:/Users/v", "relative/x", "/C:/Users/v/relative/x"},
		{"/C:/Users/v", "../x", "/C:/Users/x"},
		{"/C:/Users/v", "D:/x", "/D:/x"},
		{"/C:/", "..", "/C:/"},
		{"/home/v", "C:/../x", "/C:/x"},
		{"/home/v", "/c/x", "/c/x"},
		{"/home/v", "relative/x", "/home/v/relative/x"},
		{"/home/v", "./C:/x", "/home/v/C:/x"},
		{"/home/v", "/tmp/x", "/tmp/x"},
	} {
		if got := resolveSSHRemotePath(tc.cwd, tc.name); got != tc.want {
			t.Errorf("cwd=%q path=%q: got %q, want %q", tc.cwd, tc.name, got, tc.want)
		}
	}
	for _, name := range []string{"C:", "C:relative", `C:\Users\v\x`} {
		if _, err := parseSSHOperand("v@win:" + name); err == nil {
			t.Errorf("ambiguous remote path accepted: %q", name)
		}
		if _, err := parseSFTPBatch(strings.NewReader("mkdir '" + name + "'")); err == nil {
			t.Errorf("ambiguous batch remote path accepted: %q", name)
		}
	}
}

func TestManagedSSHParsers(t *testing.T) {
	for raw, want := range map[string]string{"host": "host", "u@host:2222": "host", "u@[2001:db8::1]:2222": "2001:db8::1"} {
		got, err := parseSSHTarget(raw)
		if err != nil || got.Host != want {
			t.Fatalf("%q => %+v %v", raw, got, err)
		}
	}
	for _, raw := range []string{"", "-o", "a\nb", "user@", "a:0", "a:65536", "a:+22", "a:22x", "::1", "[::1]junk", "host/command"} {
		if _, err := parseSSHTarget(raw); err == nil {
			t.Errorf("accepted %q", raw)
		}
	}
	for _, s := range []string{"!touch bad", "-@get a b", "lcd /tmp", "get remote", "put a b c", "reput a b", "get a* b", "get 'unterminated b"} {
		if _, err := parseSFTPBatch(strings.NewReader(s)); err == nil {
			t.Errorf("accepted batch %q", s)
		}
	}
	ops, err := parseSFTPBatch(strings.NewReader("# comment\nput 'local name' '/remote name'\nget -r /dir ./out\nquit\n"))
	if err != nil || len(ops) != 3 || ops[0].args[0] != "local name" || !ops[1].recursive {
		t.Fatalf("%+v %v", ops, err)
	}
	if _, err := parseSFTPBatch(strings.NewReader(strings.Repeat("x", sshConfigLimit+1))); err == nil {
		t.Fatal("oversize batch accepted")
	}
	for _, name := range []string{"..", "/x", "../x", `..\x`, "C:x", "NUL", "COM1.txt", "x.", "x "} {
		if safeTransferName(name) {
			t.Errorf("unsafe name accepted %q", name)
		}
	}
	for _, operand := range []string{"u@host:path", "u@[::1]:path"} {
		o, err := parseSSHOperand(operand)
		if err != nil || !o.remote {
			t.Fatalf("%+v %v", o, err)
		}
	}
	for _, operand := range []string{"./a:b", "/c/a", `C:\a`} {
		o, err := parseSSHOperand(operand)
		if err != nil || o.remote {
			t.Fatalf("%+v %v", o, err)
		}
	}
}
