package browser

import (
	"context"
	"encoding/json"
	tool "github.com/veypi/aic-pod/libs/hosts_tool"
	wire "github.com/veypi/aic-pod/protocol/hosts_tools"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestChromeLive(t *testing.T) {
	if os.Getenv("AIC_BROWSER_TEST") == "" {
		t.Skip("set AIC_BROWSER_TEST=1 to launch an isolated Chrome")
	}
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<!doctype html><title>Fixture</title><label>Email <input id="email"></label><button onclick="document.getElementById('result').textContent=document.getElementById('email').value">Save</button><button onclick="alert('confirm')">Dialog</button><button onclick="window.open('/popup')">Popup</button><button onclick="const a=document.createElement('a');a.href=window.URL.createObjectURL(new Blob(['download fixture']));a.download='fixture.txt';a.click()">Download</button><input type=file id=upload><p id=result></p>`))
	}))
	defer fixture.Close()
	root := t.TempDir()
	service := New(Config{StateDir: filepath.Join(root, "browser"), CheckFile: func(context.Context, tool.Caller, string, bool) error { return nil }})
	defer service.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	caller := tool.Caller{Subject: "owner", ConnectionID: "rtc:one", Level: 9, ExpiresAt: time.Now().Add(time.Minute)}
	d := tool.New(tool.Config{})
	if err := d.RegisterCommand(service.Tool()); err != nil {
		t.Fatal(err)
	}
	defer d.Close(ctx)
	invoke := func(method string, args any) any {
		t.Helper()
		raw, _ := json.Marshal(args)
		r := d.Handle(ctx, caller, wire.Request{Protocol: "test", ID: wire.NewID("r_"), Action: "call", Call: &wire.Invocation{Domain: "exec", Command: "browser", Method: method, Args: raw}})
		if r.Error != nil {
			t.Fatalf("%s: %v", method, r.Error)
		}
		return r.Result
	}
	page := invoke("page.create", CreateArgs{URL: fixture.URL}).(PageInfo)
	invoke("page.wait", WaitArgs{PageID: page.ID, Load: true})
	obs := invoke("page.observe", ObserveArgs{PageID: page.ID}).(Observation)
	if len(obs.Elements) == 0 {
		t.Fatal("empty AX tree")
	}
	invoke("page.fill", ActionArgs{PageID: page.ID, Locator: Locator{Label: "Email"}, Text: "hello@example.com"})
	invoke("page.click", ActionArgs{PageID: page.ID, Locator: Locator{Role: "button", Name: "Save"}})
	invoke("page.wait", WaitArgs{PageID: page.ID, Text: "hello@example.com"})
	if _, err := service.Observe(ctx, tool.Caller{Subject: "intruder"}, ObserveArgs{PageID: page.ID}); err == nil {
		t.Fatal("cross-caller page access")
	}
	caller.ConnectionID = "nats:owner" // Same service and page survive transport changes.
	if list := invoke("page.list", Empty{}).([]PageInfo); len(list) != 1 || list[0].ID != page.ID {
		t.Fatal(list)
	}
	frame, err := service.Frames(ctx, caller, PageArgs{PageID: page.ID})
	if err != nil {
		t.Fatal(err)
	}
	size := 0
	for {
		packet, e := frame.Recv(ctx)
		if e != nil {
			t.Fatal(e)
		}
		item := readFramePacket(t, packet)
		size += len(item.Data)
		if item.Final {
			break
		}
	}
	if size == 0 {
		t.Fatal("empty screenshot stream")
	}
	frame.Close()
	input, err := service.Input(ctx, caller, PageArgs{PageID: page.ID})
	if err != nil {
		t.Fatal(err)
	}
	current, _ := service.get(caller, page.ID)
	raw, _ := json.Marshal(inputBatch{Seq: 1, Document: current.snapshot().Document, Events: []inputEvent{{Type: "pointer.move", X: 1, Y: 1}}})
	if err = input.Send(ctx, raw); err != nil {
		t.Fatal(err)
	}
	if err = input.Send(ctx, raw); err == nil {
		t.Fatal("duplicate sequence accepted")
	}
	ai := caller
	ai.ConnectionID = "nats:ai"
	if _, err = service.action("click")(ctx, ai, ActionArgs{PageID: page.ID, Locator: Locator{CSS: "#email"}}); err == nil {
		t.Fatal("automation bypassed active manual input")
	}
	input.Close()
	invoke("page.click", ActionArgs{PageID: page.ID, Locator: Locator{Role: "button", Name: "Download"}})
	var downloads []download
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); {
		downloads, _ = service.DownloadList(ctx, caller, PageArgs{PageID: page.ID})
		if len(downloads) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(downloads) != 1 {
		events, _ := service.Events(ctx, caller, EventsArgs{PageID: page.ID})
		files, _ := os.ReadDir(filepath.Join(root, "browser", "downloads"))
		t.Fatalf("downloads: %+v; events=%+v; files=%+v", downloads, events, files)
	}
	completed, err := service.DownloadWait(ctx, caller, DownloadArgs{ID: downloads[0].ID})
	if err != nil || completed.State != "completed" {
		t.Fatalf("download: %+v %v", completed, err)
	}
	dest := filepath.Join(root, "export.txt")
	invoke("download.export", DownloadArgs{ID: completed.ID, Path: dest})
	data, err := os.ReadFile(dest)
	if err != nil || string(data) != "download fixture" {
		t.Fatalf("export: %q %v", data, err)
	}
	file := filepath.Join(root, "upload.txt")
	if err = os.WriteFile(file, []byte("upload"), 0600); err != nil {
		t.Fatal(err)
	}
	invoke("page.upload", UploadArgs{PageID: page.ID, Locator: Locator{CSS: "#upload"}, File: file})
	invoke("page.click", ActionArgs{PageID: page.ID, Locator: Locator{Role: "button", Name: "Popup"}})
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); {
		list, _ := service.List(ctx, caller, Empty{})
		if len(list) == 2 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	list, _ := service.List(ctx, caller, Empty{})
	if len(list) != 2 {
		t.Fatalf("popup not adopted: %+v", list)
	}
	errch := make(chan error, 1)
	go func() {
		_, err := service.action("click")(ctx, caller, ActionArgs{PageID: page.ID, Locator: Locator{Role: "button", Name: "Dialog"}})
		errch <- err
	}()
	var dialog *Dialog
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); {
		dialog = current.snapshot().Dialog
		if dialog != nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if dialog == nil {
		t.Fatal("dialog event missing")
	}
	if _, err = service.Dialog(ctx, caller, DialogArgs{PageID: page.ID, ID: dialog.ID, Accept: true}); err != nil {
		t.Fatal(err)
	}
	if err = <-errch; err != nil {
		t.Fatal(err)
	}
	old := obs.Elements[0].Ref
	invoke("page.navigate", NavigateArgs{PageID: page.ID, URL: fixture.URL + "/next"})
	invoke("page.wait", WaitArgs{PageID: page.ID, Load: true})
	_, err = current.resolve(ctx, caller, Locator{Ref: old})
	if err == nil || !strings.Contains(err.Error(), "stale_ref") {
		t.Fatalf("navigation retained ref: %v", err)
	}
	for _, p := range list {
		invoke("page.close", PageArgs{PageID: p.ID})
	}
}
