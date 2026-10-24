package host

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/veypi/aic-pod/cfg"
	"github.com/veypi/aic-pod/protocol"
)

// host 形态 exec：内建 mv 在「属主可写」的目录里必须成功。
//
// 回归（2026-10-24）：vsh 的身份键（UID/EUID）此前只是只读 shell 变量，没进
// 「命令可见环境」；内建按 POSIX 权限档判定时拿不到真实身份，回落虚拟身份
// （1000）——设备真实属主是 501，于是属主档落空、落 group 档（gid 20 == 属组
// staff），把 0755 目录误判为不可写 ⇒ 内建 mv 报 Permission denied，而同一
// 目录里原生 /bin/mv 正常、cp/rm 正常（不涉 rename 权限档）。
func TestHostBuiltinRenameInOwnerWritableDir(t *testing.T) {
	saved := cfg.Global
	cfg.Global = cfg.NewOptions()
	t.Cleanup(func() { cfg.Global = saved })

	c, _ := testClient(t)
	dir := filepath.Join(c.options().WorkDir, "sub")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	script := "mv " + filepath.Join(dir, "a") + " " + filepath.Join(dir, "b") + "\n"
	r := callTool(t, c, context.Background(), testCaller(), execRequest(script, 30000))
	res := decoded[protocol.Output](t, r.Result)
	if _, err := os.Stat(filepath.Join(dir, "b")); err != nil {
		t.Fatalf("内建 mv 未完成：%v（stderr=%q）", err, res.Attrs["stderr"])
	}
}
