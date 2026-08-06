package host

import (
	"testing"

	"github.com/veypi/aic-pod/cfg"
)

func TestOptionsOf(t *testing.T) {
	o := cfg.Options{Host: "https://ivec.ai", Key: "c"}
	opts, err := optionsOf(o, "cli", "v0.0.1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if opts.Host != "https://ivec.ai" || opts.Key != "c" || opts.DeviceType != "cli" {
		t.Fatalf("options = %+v", opts)
	}
	if opts.ExecTimeout.String() != "30m0s" {
		t.Fatalf("default timeout = %v", opts.ExecTimeout)
	}
	o.ExecTimeout = "45m"
	opts, err = optionsOf(o, "desktop", "v0.0.1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if opts.ExecTimeout.String() != "45m0s" {
		t.Fatalf("timeout = %v", opts.ExecTimeout)
	}
	o.ExecTimeout = "bogus"
	if _, err := optionsOf(o, "cli", "v0.0.1", nil); err == nil {
		t.Fatal("invalid timeout should error")
	}
}
