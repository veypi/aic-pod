package api

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/veypi/aic-pod/cfg"
)

func TestSetConfigRequiresExplicitAuthorizationRepair(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("APPDATA", dir)
	saved := cfg.Global
	t.Cleanup(func() { cfg.Global = saved })
	p, _ := cfg.Path()
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{
		"exec_policy: open\nexec_deny: [sh, 'bad rule']\nfs_allow: [/workspace]\n",
		"exec_policy: open\nexec_deny: [sh, {}]\nfs_allow: [/workspace]\n",
	} {
		if err := os.WriteFile(p, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := cfg.Load(); err != nil {
			t.Fatal(err)
		}
		view, err := GetConfig(nil)
		if err != nil || len(view.ExecDeny) == 0 || view.ExecDeny[0] == "" {
			t.Fatal("settings editor lost the malformed rule", err)
		}
		if _, err := SetConfig(nil, &SetConfigReq{HomePath: "/agents"}); err == nil {
			t.Fatal("unrelated save accepted damaged deny rules")
		}
		data, err := os.ReadFile(p)
		if err != nil || string(data) != body {
			t.Fatal("rejected save changed the file", err)
		}
		deny := []string{"sh"}
		if _, err := SetConfig(nil, &SetConfigReq{HomePath: "/agents", ExecDeny: &deny}); err != nil {
			t.Fatal(err)
		}
		o, err := cfg.LoadFile()
		if err != nil || !reflect.DeepEqual(o.ExecDeny, deny) || !reflect.DeepEqual(o.FsAllow, []string{"/workspace"}) || cfg.CheckAuth() != nil {
			t.Fatal("explicit repair did not preserve other rules", err)
		}
	}
}

func TestSetConfigDoesNotClearBrokenRuntimeOverride(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("APPDATA", dir)
	saved := cfg.Global
	t.Cleanup(func() { cfg.Global = saved })
	cfg.Global = cfg.NewOptions()
	if err := cfg.Save(cfg.Global); err != nil {
		t.Fatal(err)
	}
	cfg.Global.ExecPolicy = "dney" // e.g. an invalid environment/flag override
	for _, req := range []*SetConfigReq{{HomePath: "/agents"}, {NetPolicy: "deny"}} {
		if _, err := SetConfig(nil, req); err == nil {
			t.Fatal("unrelated update cleared malformed runtime policy")
		}
	}
	if _, err := SetConfig(nil, &SetConfigReq{ExecPolicy: "deny"}); err != nil || cfg.CheckAuth() != nil {
		t.Fatal("explicit repair failed", err)
	}
}
