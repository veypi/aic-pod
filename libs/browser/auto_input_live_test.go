package browser

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	tool "github.com/veypi/aic-pod/libs/hosts_tool"
)

func TestChromeAutomaticInputLive(t *testing.T) {
	if os.Getenv("AIC_BROWSER_TEST") == "" {
		t.Skip("set AIC_BROWSER_TEST=1 to launch an isolated Chrome")
	}
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<!doctype html><input id="text" autofocus><script>
window.keys=[];addEventListener('keydown',e=>keys.push('down:'+e.key));addEventListener('keyup',e=>keys.push('up:'+e.key));
</script>`))
	}))
	defer fixture.Close()
	s := New(Config{StateDir: t.TempDir()})
	defer s.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c := tool.Caller{Subject: "owner", ConnectionID: "rtc:input", Level: 3, ExpiresAt: time.Now().Add(time.Minute)}
	info, err := s.Create(ctx, c, CreateArgs{URL: fixture.URL})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Wait(ctx, c, WaitArgs{PageID: info.ID, Load: true}); err != nil {
		t.Fatal(err)
	}
	p, _ := s.get(c, info.ID)
	stream, err := s.Input(ctx, c, PageArgs{PageID: info.ID})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	input := stream.(*inputStream)
	controlled := func() bool { p.mu.Lock(); defer p.mu.Unlock(); return p.lease != nil }
	if controlled() {
		t.Fatal("opening channel acquired control")
	}
	seq := uint64(0)
	send := func(events ...inputEvent) {
		t.Helper()
		seq++
		raw, _ := json.Marshal(inputBatch{Seq: seq, Document: p.snapshot().Document, Events: events})
		if err := stream.Send(ctx, raw); err != nil {
			t.Fatal(err)
		}
	}
	await := func(description string, condition func() bool) {
		t.Helper()
		for !condition() {
			select {
			case <-ctx.Done():
				t.Fatal(description, ctx.Err())
			case <-time.After(20 * time.Millisecond):
			}
		}
	}
	evalTrue := func(code string) bool {
		raw, err := p.evaluate(ctx, code)
		if err != nil {
			t.Fatal(err)
		}
		return string(raw) == "true"
	}
	send(inputEvent{Type: "key.down", Key: "Shift", Code: "ShiftLeft"})
	await("first key delivery", func() bool { return evalTrue(`keys.includes('down:Shift')`) })
	if !controlled() {
		t.Fatal("real input did not acquire control")
	}
	ai := c
	ai.ConnectionID = "nats:owner"
	if err := p.write(ctx, ai, func() error { return nil }); err == nil {
		t.Fatal("AI entered active manual control")
	}
	// No heartbeat is sent. Wait for the actual ten-second idle timer.
	await("idle control release", func() bool { return !controlled() })
	if input.ctx.Err() != nil || !evalTrue(`keys.includes('up:Shift')`) {
		t.Fatal("idle cleanup closed stream or left Shift pressed")
	}
	if err := p.write(ctx, ai, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	send(inputEvent{Type: "text", Text: "again"})
	await("same stream reactivation", func() bool { return evalTrue(`text.value === 'again'`) })
	if !controlled() {
		t.Fatal("input after idle did not regain control")
	}
	send(inputEvent{Type: "key.down", Key: "Control", Code: "ControlLeft"})
	await("second key delivery", func() bool { return evalTrue(`keys.includes('down:Control')`) })
	send(inputEvent{Type: "reset"})
	await("focus reset", func() bool { return !controlled() })
	if input.ctx.Err() != nil || !evalTrue(`keys.includes('up:Control')`) {
		t.Fatal("focus reset closed stream or left Control pressed")
	}
	send(inputEvent{Type: "pointer.move", X: 1, Y: 1})
	if !controlled() {
		t.Fatal("input following reset did not regain control")
	}
	if _, err := s.Navigate(ctx, c, NavigateArgs{PageID: info.ID, URL: fixture.URL + "/next"}); err != nil {
		t.Fatal(err)
	}
	if controlled() || input.ctx.Err() == nil {
		t.Fatal("navigation retained old input")
	}
}
