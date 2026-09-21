package browser

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	tool "github.com/veypi/aic-pod/libs/hosts_tool"
)

// Exercise what an actual website sees, including the first navigation and
// targets which can start running before page.create/adoption has completed.
func TestChromeIdentityLive(t *testing.T) {
	if os.Getenv("AIC_BROWSER_TEST") == "" {
		t.Skip("set AIC_BROWSER_TEST=1 to launch an isolated Chrome")
	}
	const report = `const initialUA=navigator.userAgent, initialWebdriver=navigator.webdriver;
const initialHints=navigator.userAgentData.getHighEntropyValues(['architecture','bitness','platformVersion','fullVersionList','uaFullVersion']);
async function report() {
  const hints = await initialHints;
  return {ua:initialUA, webdriver:initialWebdriver, platform:navigator.platform,
    languages:navigator.languages, hints, headers:await (await fetch('/headers')).json()};
}`
	var mu sync.Mutex
	requests := map[string][]http.Header{}
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests[r.URL.Path] = append(requests[r.URL.Path], r.Header.Clone())
		mu.Unlock()
		w.Header().Set("Accept-CH", "Sec-CH-UA-Full-Version-List, Sec-CH-UA-Arch, Sec-CH-UA-Bitness, Sec-CH-UA-Platform-Version")
		switch r.URL.Path {
		case "/headers":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(r.Header)
		case "/worker.js", "/nested.js":
			w.Header().Set("Content-Type", "text/javascript")
			_, _ = fmt.Fprint(w, report+`onmessage = async () => postMessage(await report());`)
		case "/shared.js":
			w.Header().Set("Content-Type", "text/javascript")
			_, _ = fmt.Fprint(w, report+`onconnect = e => { const p=e.ports[0];p.onmessage=async()=>p.postMessage(await report());p.start(); };`)
		case "/sw.js":
			w.Header().Set("Content-Type", "text/javascript")
			_, _ = fmt.Fprint(w, report+`oninstall=()=>skipWaiting();onactivate=e=>e.waitUntil(clients.claim());onmessage=e=>e.waitUntil(report().then(v=>e.source.postMessage(v)));`)
		default:
			w.Header().Set("Content-Type", "text/html")
			_, _ = fmt.Fprint(w, `<!doctype html><title>Identity fixture</title><script>`+report+`window.initial=report();if(parent!==window)initial.then(report=>parent.postMessage({kind:'identity',report},'*'));</script>`)
		}
	}))
	defer fixture.Close()
	state := t.TempDir()
	s := New(Config{StateDir: state})
	defer s.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	c := tool.Caller{Subject: "owner", ConnectionID: "rtc:identity", Level: 9, ExpiresAt: time.Now().Add(time.Minute)}
	info, err := s.Create(ctx, c, CreateArgs{URL: fixture.URL, Width: 1440, Height: 900})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Wait(ctx, c, WaitArgs{PageID: info.ID, Load: true}); err != nil {
		t.Fatal(err)
	}
	p, _ := s.get(c, info.ID)
	evaluate := func(expression string, value any) {
		t.Helper()
		var response struct {
			Result struct {
				Value json.RawMessage `json:"value"`
			} `json:"result"`
			Exception json.RawMessage `json:"exceptionDetails"`
		}
		err := p.call(ctx, "Runtime.evaluate", map[string]any{"expression": expression, "returnByValue": true, "awaitPromise": true, "userGesture": true}, &response)
		if err != nil {
			t.Fatal(err)
		}
		if len(response.Exception) > 0 {
			t.Fatalf("evaluate exception: %s", response.Exception)
		}
		raw := response.Result.Value
		if err := json.Unmarshal(raw, value); err != nil {
			t.Fatalf("decode %s: %v", raw, err)
		}
	}
	type identityReport struct {
		UA, Platform string
		Webdriver    bool
		Languages    []string
		Hints        struct {
			Architecture, Bitness, Platform, PlatformVersion, UAFullVersion string
			Brands, FullVersionList                                         []struct{ Brand, Version string }
		}
		Headers http.Header
	}
	var main identityReport
	evaluate("initial", &main)
	check := func(name string, got identityReport) {
		t.Helper()
		if got.UA == "" || strings.Contains(got.UA, "Headless") || got.Webdriver || got.UA != main.UA {
			t.Fatalf("%s identity: %+v", name, got)
		}
		if got.Headers.Get("User-Agent") != got.UA || got.Hints.UAFullVersion == "" || got.Hints.Platform != main.Hints.Platform || got.Hints.Architecture != main.Hints.Architecture {
			t.Fatalf("%s inconsistent HTTP/JS identity: %+v", name, got)
		}
		if !reflect.DeepEqual(got.Hints, main.Hints) || len(got.Hints.FullVersionList) == 0 {
			t.Fatalf("%s metadata differs from main page: %+v", name, got.Hints)
		}
		for _, b := range got.Hints.Brands {
			if strings.Contains(b.Brand, "Headless") || got.Headers.Get("Sec-Ch-Ua") != "" && !strings.Contains(got.Headers.Get("Sec-Ch-Ua"), fmt.Sprintf("%q;v=%q", b.Brand, b.Version)) {
				t.Fatalf("%s inconsistent brands: %+v", name, got)
			}
		}
		for _, b := range got.Hints.FullVersionList {
			if got.Headers.Get("Sec-Ch-Ua-Full-Version-List") != "" && !strings.Contains(got.Headers.Get("Sec-Ch-Ua-Full-Version-List"), fmt.Sprintf("%q;v=%q", b.Brand, b.Version)) {
				t.Fatalf("%s inconsistent full versions: %+v", name, got)
			}
		}
	}
	if main.Headers.Get("Sec-Ch-Ua") == "" || main.Headers.Get("Sec-Ch-Ua-Full-Version-List") == "" {
		t.Fatal("main page lost Client Hints")
	}
	check("main", main)
	t.Logf("Chrome %s, platform=%s, architecture=%s, languages=%v, webdriver=%v", main.Hints.UAFullVersion, main.Hints.Platform, main.Hints.Architecture, main.Languages, main.Webdriver)
	for name, code := range map[string]string{
		"worker":            `new Promise((resolve,reject)=>{const w=new Worker('/worker.js');w.onmessage=e=>{w.terminate();resolve(e.data)};w.onerror=reject;w.postMessage('report')})`,
		"nested worker":     `new Promise((resolve,reject)=>{const w=new Worker(URL.createObjectURL(new Blob(["const w=new Worker('` + fixture.URL + `/nested.js');w.onmessage=e=>postMessage(e.data);w.postMessage('report')"],{type:'text/javascript'})));w.onmessage=e=>{w.terminate();resolve(e.data)};w.onerror=reject})`,
		"shared worker":     `new Promise((resolve,reject)=>{const w=new SharedWorker('/shared.js');w.port.onmessage=e=>{w.port.close();resolve(e.data)};w.onerror=reject;w.port.start();w.port.postMessage('report')})`,
		"service worker":    `(async()=>{const registration=await navigator.serviceWorker.register('/sw.js');await navigator.serviceWorker.ready;return new Promise(resolve=>{navigator.serviceWorker.onmessage=e=>resolve(e.data);registration.active.postMessage('report')})})()`,
		"iframe":            `new Promise(resolve=>{const f=document.createElement('iframe');f.onload=async()=>resolve(await f.contentWindow.initial);f.src='/frame';document.body.append(f)})`,
		"cross-site iframe": `new Promise(resolve=>{const url=` + jsonString(strings.Replace(fixture.URL, "127.0.0.1", "localhost", 1)) + `;const handler=e=>{if(e.origin===url&&e.data?.kind==='identity'){removeEventListener('message',handler);resolve(e.data.report)}};addEventListener('message',handler);const f=document.createElement('iframe');f.src=url+'/cross-frame';document.body.append(f)})`,
		"popup":             `new Promise(resolve=>{const w=window.open('/popup');const timer=setInterval(async()=>{if(w?.initial){clearInterval(timer);resolve(await w.initial)}},20)})`,
	} {
		t.Log("checking", name)
		var got identityReport
		evaluate(code, &got)
		check(name, got)
	}
	var size struct{ InnerWidth, InnerHeight, OuterWidth, OuterHeight, ScreenWidth, ScreenHeight, AvailWidth, AvailHeight int }
	evaluate(`({innerWidth,innerHeight,outerWidth,outerHeight,screenWidth:screen.width,screenHeight:screen.height,availWidth:screen.availWidth,availHeight:screen.availHeight})`, &size)
	if size.InnerWidth != 1440 || size.InnerHeight != 900 || size.OuterWidth < size.InnerWidth || size.OuterHeight < size.InnerHeight || size.ScreenWidth < size.OuterWidth || size.ScreenHeight < size.OuterHeight || size.AvailWidth < size.OuterWidth || size.AvailHeight < size.OuterHeight {
		t.Fatalf("inconsistent viewport/window/screen: %+v", size)
	}
	var shot struct {
		Data []byte `json:"data"`
	}
	if err := p.call(ctx, "Page.captureScreenshot", map[string]any{"format": "jpeg"}, &shot); err != nil {
		t.Fatal(err)
	}
	img, err := jpeg.DecodeConfig(bytes.NewReader(shot.Data))
	if err != nil || img.Width != 1440 || img.Height != 900 {
		t.Fatalf("screenshot: %+v %v", img, err)
	}
	// A second page with a different viewport must not resize the first window.
	if _, err = s.Create(ctx, c, CreateArgs{URL: fixture.URL + "/second", Width: 800, Height: 600}); err != nil {
		t.Fatal(err)
	}
	var after int
	evaluate("outerWidth", &after)
	if after != size.OuterWidth {
		t.Fatalf("second page resized first: %d -> %d", size.OuterWidth, after)
	}
	evaluate(`document.cookie='identity=persistent; max-age=3600';localStorage.setItem('identity','persistent');true`, new(bool))
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s = New(Config{StateDir: state})
	defer s.Close()
	info, err = s.Create(ctx, c, CreateArgs{URL: fixture.URL + "/restart"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Wait(ctx, c, WaitArgs{PageID: info.ID, Load: true}); err != nil {
		t.Fatal(err)
	}
	p, _ = s.get(c, info.ID)
	var persisted bool
	evaluate(`document.cookie.includes('identity=persistent') && localStorage.getItem('identity')==='persistent'`, &persisted)
	if !persisted {
		t.Fatal("profile state was lost across restart")
	}
	mu.Lock()
	defer mu.Unlock()
	for path, headers := range requests {
		for i, h := range headers {
			if h.Get("User-Agent") != main.UA {
				t.Errorf("request %d to %s leaked UA: %s", i+1, path, h.Get("User-Agent"))
			}
		}
	}
}
