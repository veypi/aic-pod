package hostcmd

import (
	"context"
	"encoding/json"
	"github.com/veypi/aic-pod/protocol/hosts"
	"os"
	"testing"
	"time"
)

func TestTransferReplayBudgetsAndCleanup(t *testing.T) {
	b, err := NewBytes(BytesConfig{TempDir: t.TempDir(), MaxBytes: 12, MaxSourceBytes: 12})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	conn := "old"
	m := NewTransfers(b, func(c, s string) error {
		if c != conn || s != "session" {
			return hosts.Fail("expired", "binding")
		}
		return nil
	}, TransferConfig{ProxyUploadBytes: 6, MaxStreams: 2, IdleTTL: time.Millisecond})
	defer m.Close()
	raw := func(v any) json.RawMessage { x, _ := json.Marshal(v); return x }
	params := map[string]any{"session_id": "session", "stream_id": "stream", "size": 6}
	value, err := m.Open(conn, false, true, raw(params))
	if err != nil {
		t.Fatal(err)
	}
	item := value.(hosts.StreamItem)
	retry, err := m.Open(conn, true, true, raw(params))
	if err != nil || retry != value {
		t.Fatal("idempotent allocation", err)
	}
	params["size"] = 5
	if _, err = m.Open(conn, false, true, raw(params)); err == nil {
		t.Fatal("conflicting stream allocation")
	}
	h := hosts.Frame{RequestID: "r1", SessionID: "session", StreamID: item.StreamID, ItemID: item.ItemID, Seq: 0, Offset: 0}
	if _, err = m.Write(conn, h, []byte("abc"), false); err != nil {
		t.Fatal(err)
	}
	conn = "new"
	if _, err = m.Write("old", h, []byte("abc"), false); err == nil {
		t.Fatal("old binding accepted")
	}
	h.RequestID = "retry"
	if _, err = m.Write(conn, h, []byte("abc"), true); err != nil {
		t.Fatal(err)
	}
	if _, err = m.Write(conn, h, []byte("xyz"), true); err == nil {
		t.Fatal("conflicting duplicate accepted")
	}
	h.Seq = 1
	h.Offset = 3
	h.Final = true
	if _, err = m.Write(conn, h, []byte("def"), true); err != nil {
		t.Fatal(err)
	}
	seal := map[string]any{"session_id": "session", "stream_id": "stream", "actual_size": 6, "sha256": "wrong"}
	if _, err = m.Seal(conn, raw(seal)); err == nil {
		t.Fatal("checksum mismatch accepted")
	}
	delete(seal, "sha256")
	source, err := m.Seal(conn, raw(seal))
	if err != nil {
		t.Fatal(err)
	}
	if err = m.Cancel(conn, "session", "stream"); err != nil {
		t.Fatal(err)
	}
	descriptor := source.(ByteSource)
	if _, err = b.Describe("session", descriptor.Ref); err != nil {
		t.Fatal("cancel destroyed sealed source")
	}
	params["stream_id"] = "large"
	params["size"] = 7
	if _, err = m.Open(conn, true, true, raw(params)); err == nil {
		t.Fatal("proxy budget ignored")
	}
	value, err = m.Open(conn, false, true, raw(params))
	if err != nil {
		t.Fatal(err)
	}
	big := value.(hosts.StreamItem)
	h = hosts.Frame{SessionID: "session", StreamID: big.StreamID, ItemID: big.ItemID, Seq: 0}
	if _, err = m.Write(conn, h, []byte("x"), true); err == nil {
		t.Fatal("takeover bypassed proxy budget")
	}
	if _, err = m.Write(conn, h, []byte("1234567"), false); err == nil {
		t.Fatal("shared byte storage budget ignored")
	}
	m.CloseSession("session")
	b.CloseSession("session")
	if b.used != 0 || b.pending != 0 || len(m.streams) != 0 {
		t.Fatal("upload reservations leaked", b.used, b.pending)
	}
	files, err := os.ReadDir(b.dir)
	if err != nil || len(files) != 0 {
		t.Fatal("temp files leaked", files, err)
	}
	// Invalid session lookup fails before any source access.
	if _, err = m.Pull(context.Background(), "old", raw(map[string]any{"session_id": "session", "stream_id": "large"}), "pull"); err == nil {
		t.Fatal("closed stream accessible")
	}
}
