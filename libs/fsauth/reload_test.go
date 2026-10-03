package fsauth

import (
	"github.com/veypi/aic-pod/cfg"
	"github.com/veypi/vbox"
	"testing"
)

func TestFailedReloadPreservesCompletePolicy(t *testing.T) {
	saved := cfg.Global
	t.Cleanup(func() { cfg.Global = saved })
	cfg.Global = cfg.NewOptions()
	cfg.Global.FsRules = []string{"deny:/protected"}
	p := mustNewPolicy(t)
	cfg.Global.FsPolicy = cfg.PolicyOpen
	cfg.Global.FsRules = []string{"rw:/workspace", "deny:$AIC_MISSING_RULE_ROOT/**"}
	if err := p.Reconcile(); err == nil {
		t.Fatal("invalid table accepted")
	}
	if p.OpenMode() || p.Snapshot("").Match("/protected/key", vbox.OpRead).Allow {
		t.Fatal("failed reload partially replaced policy")
	}
}
