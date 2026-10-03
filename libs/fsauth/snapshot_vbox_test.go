package fsauth

// snapshot_vbox_test.go 语义映射回归（old last-wins + 表外便利根 →
// new first-wins 全表化）：覆盖关系保持是 2.7.2 的核心风险点。

import (
	"path/filepath"
	"testing"

	"github.com/veypi/aic-pod/cfg"
	"github.com/veypi/vbox"
)

// TestSnapshotCfgOverridesBuiltinDeny cfg 行可覆盖 builtin deny（old：表尾
// 后命中胜；new：cfg 排在 builtin deny 前首命中）——覆盖关系必须在两种
// 语义下一致。
func TestSnapshotCfgOverridesBuiltinDeny(t *testing.T) {
	old := cfg.AuthSnapshot()
	t.Cleanup(func() { cfg.SetAuth(old) })
	a := old
	a.FsPolicy = cfg.PolicyDeny
	// builtin deny 含 SSH 私钥类（默认表）；选一条确定在内建表里的目标验证。
	denied := defaultDenyPaths()
	if len(denied) == 0 {
		t.Skip("no builtin deny on this platform")
	}
	target := denied[0]
	a.FsRules = []string{"rw:" + target}
	cfg.SetAuth(a)

	p := mustNewPolicy(t)
	snap := p.Snapshot("s1")
	d := snap.Match(target, vbox.OpWrite)
	if !d.Allow {
		t.Fatalf("cfg rw should override builtin deny under first-wins: %+v", d)
	}
	// 未覆盖的 deny 目标仍拒。
	if len(denied) > 1 {
		if d2 := snap.Match(denied[1], vbox.OpWrite); d2.Allow {
			t.Fatalf("uncovered builtin deny should hold: %s %+v", denied[1], d2)
		}
		if d3 := snap.Match(denied[1], vbox.OpRead); d3.Allow {
			t.Fatalf("deny row should block read too: %s", denied[1])
		}
	}
}

// TestSnapshotCfgFirstRowWins permanent 追加语义钉死（todo 3.4.3）：新行
// append 到 config <域>_rules 文件尾 → cfg 组内反转（old last-wins → new
// first-wins 映射）使其位于 cfg 段最前 → 首命中压过同段更早的 deny 行。
func TestSnapshotCfgFirstRowWins(t *testing.T) {
	old := cfg.AuthSnapshot()
	t.Cleanup(func() { cfg.SetAuth(old) })
	a := old
	a.FsPolicy = cfg.PolicyDeny
	dir := t.TempDir()
	// 文件行序 = persistGrant 落盘形态：deny 在先、rw 追加在尾。
	a.FsRules = []string{"rw:" + dir, "deny:" + dir}
	cfg.SetAuth(a)
	p := mustNewPolicy(t)
	if d := p.Snapshot("s1").Match(dir+string(filepath.Separator)+"x", vbox.OpWrite); !d.Allow {
		t.Fatalf("appended tail row should win within cfg segment under first-wins: %+v", d)
	}
}

// TestSnapshotTempGrantBeatsAll temp grant 插表头压一切（含 builtin deny——
// 2.7.4 DenyHit 拒批删除后的新语义：行序表达，无硬底线）。
func TestSnapshotTempGrantBeatsAll(t *testing.T) {
	old := cfg.AuthSnapshot()
	t.Cleanup(func() { cfg.SetAuth(old) })
	a := old
	a.FsPolicy = cfg.PolicyDeny
	cfg.SetAuth(a)
	p := mustNewPolicy(t)
	denied := defaultDenyPaths()
	if len(denied) == 0 {
		t.Skip("no builtin deny on this platform")
	}
	p.Grant("s1", denied[0])
	if d := p.Snapshot("s1").Match(denied[0], vbox.OpWrite); !d.Allow {
		t.Fatalf("temp grant should top the table: %+v", d)
	}
	// 别的 sid 不受影响（会话级）。
	if d := p.Snapshot("s2").Match(denied[0], vbox.OpWrite); d.Allow {
		t.Fatalf("temp grant must be session-scoped")
	}
}

// TestSnapshotConvenienceBelowDeny 便利根 rw 不压 builtin deny（行尾位置）。
func TestSnapshotConvenienceBelowDeny(t *testing.T) {
	old := cfg.AuthSnapshot()
	t.Cleanup(func() { cfg.SetAuth(old) })
	a := old
	a.FsPolicy = cfg.PolicyDeny
	cfg.SetAuth(a)
	p := mustNewPolicy(t)
	wd := t.TempDir()
	p.SetWorkDir(wd)
	// 便利根内正常放行。
	inner := filepath.Join(wd, "ok.txt")
	if d := p.Snapshot("s1").Match(inner, vbox.OpWrite); !d.Allow {
		t.Fatalf("workdir should be writable: %+v", d)
	}
	// 出便利根未命中 → DefaultWrite deny。
	if d := p.Snapshot("s1").Match("/definitely/not/covered/path", vbox.OpWrite); d.Allow {
		t.Fatalf("default write should be deny in closed mode")
	}
	if d := p.Snapshot("s1").Match("/definitely/not/covered/path", vbox.OpRead); !d.Allow {
		t.Fatalf("read default open")
	}
}

// TestSnapshotOpenMode fs_policy=open → DefaultWrite rw。
func TestSnapshotOpenMode(t *testing.T) {
	old := cfg.AuthSnapshot()
	t.Cleanup(func() { cfg.SetAuth(old) })
	a := old
	a.FsPolicy = cfg.PolicyOpen
	cfg.SetAuth(a)
	p := mustNewPolicy(t)
	if d := p.Snapshot("").Match("/anywhere/else", vbox.OpWrite); !d.Allow {
		t.Fatalf("open mode should allow unmatched writes")
	}
}

// TestSnapshotForNativeWorkspaceMetadata 工作区元数据保护（内置 ro 行）：
// 存在才下发、git 豁免、cfg/temp 可覆盖、便利根不可压；普通 Snapshot 不下发。
func TestSnapshotForNativeWorkspaceMetadata(t *testing.T) {
	wd := t.TempDir()
	p := newTestPolicy(t, wd)
	mkdir(t, filepath.Join(wd, ".git"), filepath.Join(wd, ".aws"))
	gitFile := filepath.Join(wd, ".git", "HEAD")
	awsFile := filepath.Join(wd, ".aws", "credentials")

	// 普通命令：.git / .aws 写拒、读放行（便利根在表尾，压不过保护行）。
	if d := p.SnapshotForNative("s1", wd, "bash").Match(gitFile, vbox.OpWrite); d.Allow {
		t.Fatalf("native snapshot must deny writes under .git: %+v", d)
	}
	if d := p.SnapshotForNative("s1", wd, "bash").Match(awsFile, vbox.OpWrite); d.Allow {
		t.Fatalf(".aws must be protected: %+v", d)
	}
	if d := p.SnapshotForNative("s1", wd, "bash").Match(gitFile, vbox.OpRead); !d.Allow {
		t.Fatalf(".git reads stay open: %+v", d)
	}
	// 进程内门快照（Snapshot）不下发保护（维持现状）。
	if d := p.Snapshot("s1").Match(gitFile, vbox.OpWrite); !d.Allow {
		t.Fatalf("plain snapshot must not carry protection: %+v", d)
	}
	// git 命令豁免；不存在保护目录不下发。
	if d := p.SnapshotForNative("s1", wd, "git").Match(gitFile, vbox.OpWrite); !d.Allow {
		t.Fatalf("git invocation must be exempt: %+v", d)
	}
	if d := p.SnapshotForNative("s1", wd, "bash").Match(filepath.Join(wd, ".codex", "x"), vbox.OpWrite); !d.Allow {
		t.Fatalf("absent metadata dirs must stay writable: %+v", d)
	}
	// cfg rw 行（更靠表头）可显式覆盖。
	setRules(t, p, "rw:"+filepath.Join(wd, ".git"))
	if d := p.SnapshotForNative("s1", wd, "bash").Match(gitFile, vbox.OpWrite); !d.Allow {
		t.Fatalf("explicit cfg rw row must override protection: %+v", d)
	}
	// temp grant（表头）同样覆盖。
	p.Grant("s2", filepath.Join(wd, ".aws"))
	if d := p.SnapshotForNative("s2", wd, "bash").Match(awsFile, vbox.OpWrite); !d.Allow {
		t.Fatalf("temp grant must override protection: %+v", d)
	}
}
