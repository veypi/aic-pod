package browser

import (
	"context"
	"encoding/json"
	tool "github.com/veypi/aic-pod/libs/hosts_tool"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestChromeUploadAndLocatorWaitLive(t *testing.T) {
	if os.Getenv("AIC_BROWSER_TEST") == "" {
		t.Skip("set AIC_BROWSER_TEST=1 to launch an isolated Chrome")
	}
	const body = "review fixture: upload must survive the call"
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/submit" {
			f, _, err := r.FormFile("file")
			if err != nil {
				http.Error(w, err.Error(), 400)
				return
			}
			defer f.Close()
			io.Copy(w, f)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		io.WriteString(w, `<!doctype html><button class=dup>same</button><button class=dup>same</button><button id=visible>visible</button><button id=hidden style="display:none">hidden</button><input type=file id=upload>`)
	}))
	defer fixture.Close()
	root := t.TempDir()
	s := New(Config{StateDir: filepath.Join(root, "browser"), CheckFile: func(context.Context, tool.Caller, string, bool) error { return nil }})
	defer s.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Second)
	defer cancel()
	c := tool.Caller{Subject: "review-owner", Level: 9, ConnectionID: "review", ExpiresAt: time.Now().Add(time.Minute)}
	page, err := s.Create(ctx, c, CreateArgs{URL: fixture.URL})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Wait(ctx, c, WaitArgs{PageID: page.ID, Load: true}); err != nil {
		t.Fatal(err)
	}
	for _, css := range []string{"#missing", "#hidden"} {
		if _, err := s.Wait(ctx, c, WaitArgs{PageID: page.ID, Locator: &Locator{CSS: css}, State: "hidden", TimeoutMS: 250}); err != nil {
			t.Fatalf("%s: %v", css, err)
		}
	}
	if _, err := s.Wait(ctx, c, WaitArgs{PageID: page.ID, Locator: &Locator{CSS: "#visible"}, State: "hidden", TimeoutMS: 250}); err == nil {
		t.Fatal("visible element satisfies hidden")
	}
	t.Run("hidden_multiple_visible_matches", func(t *testing.T) {
		_, err := s.Wait(ctx, c, WaitArgs{PageID: page.ID, Locator: &Locator{CSS: ".dup"}, State: "hidden", TimeoutMS: 250})
		if err == nil {
			t.Error("BUG: two visible matches satisfy hidden")
		} else {
			t.Logf("correctly rejected: %v", err)
		}
	})
	source := filepath.Join(root, "fixture.txt")
	if err = os.WriteFile(source, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	readAndSubmit := func() (json.RawMessage, error) {
		r, err := s.Evaluate(ctx, c, EvaluateArgs{PageID: page.ID, Code: `(async()=>{const f=document.querySelector('#upload').files[0];const out={size:f.size};try{out.text=await f.text()}catch(e){out.read_error=e.name}const fd=new FormData();fd.append('file',f);try{const r=await fetch('/submit',{method:'POST',body:fd,signal:AbortSignal.timeout(3000)});out.submitted=await r.text();out.status=r.status}catch(e){out.submit_error=e.name}return out})()`})
		if err != nil {
			return nil, err
		}
		return json.Marshal(r.Data)
	}
	// Control: Chrome keeps access while the original path still exists.
	p, err := s.get(c, page.ID)
	if err != nil {
		t.Fatal(err)
	}
	object, err := p.resolve(ctx, c, Locator{CSS: "#upload"})
	if err != nil {
		t.Fatal(err)
	}
	if err = p.call(ctx, "DOM.setFileInputFiles", map[string]any{"files": []string{source}, "objectId": object}, nil); err != nil {
		t.Fatal(err)
	}
	p.release(object)
	raw, err := readAndSubmit()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("control existing path: %s", raw)
	var control struct {
		Text, Submitted string
		Status          int
	}
	if err = json.Unmarshal(raw, &control); err != nil || control.Text != body || control.Submitted != body || control.Status != 200 {
		t.Fatalf("control failed: %s %v", raw, err)
	}
	t.Run("upload_then_read_and_submit", func(t *testing.T) {
		if _, err := s.Upload(ctx, c, UploadArgs{PageID: page.ID, Locator: Locator{CSS: "#upload"}, File: source}); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(source); err != nil {
			t.Fatal(err)
		}
		raw, err := readAndSubmit()
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("after Upload returns: %s", raw)
		var got struct {
			Text, Submitted string
			Status          int
		}
		if err = json.Unmarshal(raw, &got); err != nil || got.Text != body || got.Submitted != body || got.Status != 200 {
			t.Errorf("BUG: uploaded file cannot be read and submitted after Upload returns: %s", raw)
		}
	})
	if _, err := s.ClosePage(ctx, c, PageArgs{PageID: page.ID}); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(root, "browser", "uploads"))
	if err != nil || len(entries) != 0 {
		t.Fatal("page close retained upload files", err)
	}
}
