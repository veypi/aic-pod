package host

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/veypi/aic-pod/libs/proto"
	"github.com/veypi/aic-pod/protocol/ui"
)

func TestUIRequestJournal(t *testing.T) {
	c := &Client{uiSessionRoot: t.TempDir()}
	req := &proto.ToolRequest{SessionID: "test", MsgID: "action1", Data: json.RawMessage(`{"action":"browser","argv":["click","@sabc:e1"]}`)}
	argv := []string{"click", "@sabc:e1", "--format", "json"}
	var count atomic.Int32
	run := func() *proto.ToolResponse {
		count.Add(1)
		o, _ := ui.Parse("browser", argv)
		r := ui.NewResult(o)
		r.Action = map[string]any{"performed": true}
		return uiResponse(req.MsgID, o, r, c.uiWorkDir(req.SessionID))
	}
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := c.runUIOnce(context.Background(), req, "browser", argv, run)
			if r.State != proto.StateCompleted {
				t.Errorf("%+v", r)
			}
		}()
	}
	wg.Wait()
	if count.Load() != 1 {
		t.Fatalf("replayed %d times", count.Load())
	}
	// A new Client simulates a restarted host and must return the durable response.
	other := &Client{uiSessionRoot: c.uiSessionRoot}
	other.runUIOnce(context.Background(), req, "browser", argv, run)
	if count.Load() != 1 {
		t.Fatal("restart replay")
	}
	conflict := *req
	conflict.Data = json.RawMessage(`{"different":true}`)
	r := other.runUIOnce(context.Background(), &conflict, "browser", argv, run)
	var body ui.Result
	json.Unmarshal([]byte(r.Content), &body)
	if body.Error.Code != "request_conflict" {
		t.Fatal(r.Content)
	}
	// A pending record models process death after an effect but before result persistence.
	key := sha256.Sum256([]byte(req.MsgID))
	payload := sha256.Sum256(req.Data)
	b, _ := json.Marshal(uiJournal{Hash: hex.EncodeToString(payload[:])})
	file := filepath.Join(c.uiWorkDir(req.SessionID), ".ui", "requests", hex.EncodeToString(key[:])+".json")
	os.WriteFile(file, b, 0600)
	r = c.runUIOnce(context.Background(), req, "browser", argv, run)
	json.Unmarshal([]byte(r.Content), &body)
	if body.Error.Code != "outcome_unknown" || count.Load() != 1 {
		t.Fatal(r.Content)
	}
}

func TestUIShellLostReplyHasUnknownOutcome(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		bufio.NewReader(conn).ReadString('\n')
	}()
	req := &proto.ToolRequest{MsgID: "lost-reply", GrantedLevel: 2, Deadline: time.Now().Add(time.Second).Format(time.RFC3339)}
	response := RunViaShell(ShellChannel{Addr: listener.Addr().String(), Token: "test"})(context.Background(), "ui-lost-reply", req, []string{"click", "@sabc:e1", "--format", "json"})
	<-done
	var result ui.Result
	if json.Unmarshal([]byte(response.Content), &result) != nil {
		t.Fatal(response.Content)
	}
	if result.Action["performed"] != "unknown" || result.Error.Code != "transport_failed" {
		t.Fatal(response.Content)
	}
}
func TestUIResultBudgetAndFullArtifact(t *testing.T) {
	o, _ := ui.Parse("browser", []string{"read", "--format", "json"})
	r := ui.NewResult(o)
	body := strings.Repeat("中文 text ", 10000)
	r.Data = map[string]any{"text": body}
	response := uiResponse("budget", o, r, t.TempDir())
	if len(response.Content) > 12*1024 || !json.Valid([]byte(response.Content)) {
		t.Fatal("invalid inline result")
	}
	if len(r.Artifacts) != 1 {
		t.Fatal("full result not retained")
	}
	b, err := os.ReadFile(r.Artifacts[0]["path"].(string))
	if err != nil {
		t.Fatal(err)
	}
	var full ui.Result
	json.Unmarshal(b, &full)
	if full.Data.(map[string]any)["text"] != body {
		t.Fatal("artifact lost content")
	}
	root := filepath.Join(t.TempDir(), "not-a-directory")
	os.WriteFile(root, []byte("x"), 0600)
	r = ui.NewResult(o)
	r.Data = map[string]any{"text": body}
	response = uiResponse("budget", o, r, root)
	if len(response.Content) > 12*1024 || !json.Valid([]byte(response.Content)) {
		t.Fatal("artifact failure broke protocol")
	}
}
