package browser

import (
	"context"
	"encoding/json"
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
	service := New(Config{StateDir: filepath.Join(root, "browser")})
	defer service.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	page, err := service.Create(ctx, CreateArgs{URL: fixture.URL})
	if err != nil {
		t.Fatal(err)
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	_, err = service.Wait(ctx, WaitArgs{PageID: page.ID, Load: true})
	must(err)
	obs, err := service.Observe(ctx, ObserveArgs{PageID: page.ID})
	must(err)
	if len(obs.Elements) == 0 {
		t.Fatal("empty AX tree")
	}
	_, err = service.action("fill")(ctx, ActionArgs{PageID: page.ID, Locator: Locator{Label: "Email"}, Text: "hello@example.com"})
	must(err)
	_, err = service.action("click")(ctx, ActionArgs{PageID: page.ID, Locator: Locator{Role: "button", Name: "Save"}})
	must(err)
	_, err = service.Wait(ctx, WaitArgs{PageID: page.ID, Text: "hello@example.com"})
	must(err)
	if list, err := service.List(ctx, Empty{}); err != nil || len(list) != 1 || list[0].ID != page.ID {
		t.Fatal(list, err)
	}
	frame, err := service.Frames(ctx, PageArgs{PageID: page.ID})
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
	input, err := service.Input(ctx, "viewer-1", PageArgs{PageID: page.ID})
	if err != nil {
		t.Fatal(err)
	}
	current, _ := service.get(page.ID)
	raw, _ := json.Marshal(inputBatch{Seq: 1, Document: current.snapshot().Document, Events: []inputEvent{{Type: "pointer.move", X: 1, Y: 1}}})
	if err = input.Send(ctx, raw); err != nil {
		t.Fatal(err)
	}
	if err = input.Send(ctx, raw); err == nil {
		t.Fatal("duplicate sequence accepted")
	}
	// 输入租约持有期间，自动化动作一律 control_busy。
	if _, err = service.action("click")(ctx, ActionArgs{PageID: page.ID, Locator: Locator{CSS: "#email"}}); err == nil {
		t.Fatal("automation bypassed active manual input")
	}
	input.Close()
	_, err = service.action("click")(ctx, ActionArgs{PageID: page.ID, Locator: Locator{Role: "button", Name: "Download"}})
	must(err)
	var downloads []download
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); {
		downloads, _ = service.DownloadList(ctx, PageArgs{PageID: page.ID})
		if len(downloads) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(downloads) != 1 {
		events, _ := service.Events(ctx, EventsArgs{PageID: page.ID})
		files, _ := os.ReadDir(filepath.Join(root, "browser", "downloads"))
		t.Fatalf("downloads: %+v; events=%+v; files=%+v", downloads, events, files)
	}
	completed, err := service.DownloadWait(ctx, DownloadArgs{ID: downloads[0].ID})
	if err != nil || completed.State != "completed" {
		t.Fatalf("download: %+v %v", completed, err)
	}
	dest := filepath.Join(root, "export.txt")
	if _, err := service.DownloadExport(ctx, DownloadArgs{ID: completed.ID, Path: dest}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(dest)
	if err != nil || string(data) != "download fixture" {
		t.Fatalf("export: %q %v", data, err)
	}
	file := filepath.Join(root, "upload.txt")
	if err = os.WriteFile(file, []byte("upload"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err = service.Upload(ctx, UploadArgs{PageID: page.ID, Locator: Locator{CSS: "#upload"}, File: file})
	must(err)
	_, err = service.action("click")(ctx, ActionArgs{PageID: page.ID, Locator: Locator{Role: "button", Name: "Popup"}})
	must(err)
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); {
		list, _ := service.List(ctx, Empty{})
		if len(list) == 2 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	list, _ := service.List(ctx, Empty{})
	if len(list) != 2 {
		t.Fatalf("popup not adopted: %+v", list)
	}
	errch := make(chan error, 1)
	go func() {
		_, err := service.action("click")(ctx, ActionArgs{PageID: page.ID, Locator: Locator{Role: "button", Name: "Dialog"}})
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
	if _, err = service.Dialog(ctx, DialogArgs{PageID: page.ID, ID: dialog.ID, Accept: true}); err != nil {
		t.Fatal(err)
	}
	if err = <-errch; err != nil {
		t.Fatal(err)
	}
	old := obs.Elements[0].Ref
	_, err = service.Navigate(ctx, NavigateArgs{PageID: page.ID, URL: fixture.URL + "/next"})
	must(err)
	_, err = service.Wait(ctx, WaitArgs{PageID: page.ID, Load: true})
	must(err)
	_, err = current.resolve(ctx, Locator{Ref: old})
	if err == nil || !strings.Contains(err.Error(), "stale_ref") {
		t.Fatalf("navigation retained ref: %v", err)
	}
	for _, p := range list {
		if _, err := service.ClosePage(ctx, PageArgs{PageID: p.ID}); err != nil {
			t.Fatal(err)
		}
	}
}
