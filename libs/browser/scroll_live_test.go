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

func TestChromeScrollLive(t *testing.T) {
	if os.Getenv("AIC_BROWSER_TEST") == "" {
		t.Skip("set AIC_BROWSER_TEST=1 to launch an isolated Chrome")
	}
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<!doctype html><style>
body{margin:0;height:4000px;background:linear-gradient(white,teal)}
#inner{position:fixed;left:20px;top:20px;width:240px;height:200px;overflow:auto;overscroll-behavior:contain;background:white}
#content{height:2000px;background:linear-gradient(red,blue)}
</style><div id=inner><div id=content></div></div><script>
window.wheels=[];window.positions=[];
addEventListener('wheel',e=>wheels.push(e.deltaY),{passive:true});
inner.addEventListener('scroll',()=>positions.push(inner.scrollTop));
</script>`))
	}))
	defer fixture.Close()
	s := New(Config{StateDir: t.TempDir(), Width: 800, Height: 600})
	defer s.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	c := tool.Caller{Subject: "owner", ConnectionID: "rtc:scroll", Level: 9, ExpiresAt: time.Now().Add(time.Minute)}
	info, err := s.Create(ctx, c, CreateArgs{URL: fixture.URL})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Wait(ctx, c, WaitArgs{PageID: info.ID, Load: true}); err != nil {
		t.Fatal(err)
	}
	p, err := s.get(c, info.ID)
	if err != nil {
		t.Fatal(err)
	}
	input, err := s.Input(ctx, c, PageArgs{PageID: info.ID})
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	seq := uint64(0)
	send := func(events ...inputEvent) {
		t.Helper()
		seq++
		raw, _ := json.Marshal(inputBatch{Seq: seq, Document: p.snapshot().Document, Events: events})
		if err := input.Send(ctx, raw); err != nil {
			t.Fatal(err)
		}
	}
	read := func() struct {
		Inner, Outer      float64
		Wheels, Positions []float64
	} {
		t.Helper()
		// Let compositor scrolling settle; record the native scroll positions too.
		raw, err := p.evaluate(ctx, `new Promise(resolve=>{let last='',stable=0;function sample(){const v=JSON.stringify([inner.scrollTop,scrollY]);stable=v===last?stable+1:0;last=v;if(stable>=5)resolve({Inner:inner.scrollTop,Outer:scrollY,Wheels:wheels,Positions:positions});else requestAnimationFrame(sample)}sample()})`)
		if err != nil {
			t.Fatal(err)
		}
		var state struct {
			Inner, Outer      float64
			Wheels, Positions []float64
		}
		if err := json.Unmarshal(raw, &state); err != nil {
			t.Fatal(err)
		}
		return state
	}
	// Simulate an AI action holding the page. The old wheel must remain
	// replaceable until native dispatch can begin, including a direction change.
	p.gate <- struct{}{}
	send(inputEvent{Type: "wheel", X: 238, Y: 218, DeltaY: 100})
	time.Sleep(30 * time.Millisecond)
	send(inputEvent{Type: "wheel", X: 238, Y: 218, DeltaY: -20})
	<-p.gate
	state := read()
	if len(state.Wheels) != 1 || state.Wheels[0] != -20 || state.Inner != 0 {
		t.Fatalf("stale queued wheel replayed: %+v", state)
	}
	if _, err := p.evaluate(ctx, `wheels=[];positions=[]`); err != nil {
		t.Fatal(err)
	}
	wheel := func(dy float64) inputEvent { return inputEvent{Type: "wheel", X: 238, Y: 218, DeltaY: dy} }
	burst := make([]inputEvent, 32)
	for n := range burst {
		burst[n] = wheel(4)
	}
	send(burst...)
	state = read()
	if state.Inner != 4 || state.Outer != 0 || len(state.Wheels) != 1 || state.Wheels[0] != 4 {
		t.Fatalf("burst: %+v", state)
	}
	// Holding the wheel at the bottom edge must remain monotonic and must not
	// leak scroll to the surrounding document.
	for range 12 {
		send(wheel(200))
		read()
	}
	state = read()
	if state.Inner != 1800 || state.Outer != 0 {
		t.Fatalf("bottom edge: %+v", state)
	}
	for n := 1; n < len(state.Positions); n++ {
		if state.Positions[n] < state.Positions[n-1] {
			t.Fatalf("scroll reversed at edge: %v", state.Positions)
		}
	}
	before := len(state.Wheels)
	send(wheel(-120), wheel(40))
	state = read()
	if len(state.Wheels) != before+1 || state.Wheels[len(state.Wheels)-1] != 40 || state.Inner != 1800 {
		t.Fatalf("old wheel replayed before newest: %+v", state)
	}
	send(wheel(-120))
	state = read()
	if state.Inner != 1680 {
		t.Fatalf("fresh reverse wheel: %+v", state)
	}
	send(wheel(40))
	state = read()
	if state.Inner != 1720 {
		t.Fatalf("fresh forward wheel: %+v", state)
	}
	innerAfterReversal := state.Inner
	// The next gesture can target the outer viewport near its lower edge.
	send(inputEvent{Type: "wheel", X: 600, Y: 598, DeltaY: 160})
	state = read()
	if state.Outer != 160 || state.Inner != innerAfterReversal {
		t.Fatalf("outer: %+v", state)
	}
}
