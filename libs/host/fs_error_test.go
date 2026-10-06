package host

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/veypi/aic-pod/libs/fsx"
	"github.com/veypi/aic-pod/protocol"
	"github.com/veypi/vigo/contrib/ufs"
)

// TestFSFaultClassifiesFSError fs 操作错误经 fsx 包装后仍可判定：错误链穿到
// syscall ENOENT → not_found（而非 internal），且报错保留调用者路径。
func TestFSFaultClassifiesFSError(t *testing.T) {
	backing, err := ufs.NewLocalFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	env := &fsx.Env{FS: backing, Workdir: "/"}
	raw, _ := json.Marshal(map[string]any{"action": "read", "path": "/dir/nope.txt"})
	_, err = fsx.RunFS(context.Background(), env, raw)
	if err == nil {
		t.Fatal("read missing file: want error")
	}

	var fault *protocol.Fault
	classified := fsFault(err)
	if !errors.As(classified, &fault) {
		t.Fatalf("fsFault = %v (%T); want *protocol.Fault", classified, classified)
	}
	if fault.Code != "not_found" {
		t.Fatalf("fault code = %q; want not_found", fault.Code)
	}
	if !strings.Contains(fault.Message, "/dir/nope.txt") {
		t.Fatalf("fault message 未回显调用者路径: %q", fault.Message)
	}
	if fault.Effect != "none" {
		t.Fatalf("fault effect = %q; want none", fault.Effect)
	}
}

// TestFSFaultLeavesNonFSErrors 非 FS 类错误原样透出（交给 AsFault 判 internal）。
func TestFSFaultLeavesNonFSErrors(t *testing.T) {
	err := errors.New("boom")
	if got := fsFault(err); got != err {
		t.Fatalf("fsFault(non-fs) = %v; want 原错误", got)
	}
	if got := fsFault(nil); got != nil {
		t.Fatalf("fsFault(nil) = %v; want nil", got)
	}
}
