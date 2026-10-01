package skillrun

import (
	"strings"
	"testing"
)

func TestManifestValidate(t *testing.T) {
	good := func() *Manifest {
		return &Manifest{Providers: []Provider{{ID: "main", Kind: KindProcess, Entry: "cli/bin/x"}}}
	}
	cases := []struct {
		name    string
		mutate  func(m *Manifest)
		wantErr string
	}{
		{"ok", func(m *Manifest) {}, ""},
		{"ok with stream", func(m *Manifest) {
			m.Streams = []StreamDecl{{Name: "frames", Provider: "main"}}
		}, ""},
		{"empty providers", func(m *Manifest) { m.Providers = nil }, "providers is required"},
		{"empty id", func(m *Manifest) { m.Providers[0].ID = "" }, "id is required"},
		{"dup id", func(m *Manifest) { m.Providers = append(m.Providers, m.Providers[0]) }, "duplicate provider id"},
		{"bad kind", func(m *Manifest) { m.Providers[0].Kind = "daemon" }, "must be process|service"},
		{"empty entry", func(m *Manifest) { m.Providers[0].Entry = "" }, "entry: is required"},
		{"abs entry", func(m *Manifest) { m.Providers[0].Entry = "/usr/bin/x" }, "must be relative"},
		{"escape entry", func(m *Manifest) { m.Providers[0].Entry = "../outside/x" }, "escapes package dir"},
		{"nested escape", func(m *Manifest) { m.Providers[0].Entry = "cli/../../outside" }, "escapes package dir"},
		{"stream empty name", func(m *Manifest) {
			m.Streams = []StreamDecl{{Name: "", Provider: "main"}}
		}, "name is required"},
		{"stream bad provider", func(m *Manifest) {
			m.Streams = []StreamDecl{{Name: "frames", Provider: "ghost"}}
		}, "not found"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := good()
			c.mutate(m)
			err := m.Validate()
			if c.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("Validate() = %v, want containing %q", err, c.wantErr)
			}
		})
	}
}
