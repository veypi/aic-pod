package host

import (
	"context"
	"encoding/json"
	"github.com/veypi/aic-pod/cfg"
	"github.com/veypi/aic-pod/libs/browser"
	"github.com/veypi/aic-pod/libs/exec_procs"
	wire "github.com/veypi/aic-pod/protocol/hosts_tools"
	"os"
	"testing"
	"time"
)

func TestChromeWaitUsesSharedExecutionWithoutClosingPage(t *testing.T) {
	if os.Getenv("AIC_BROWSER_TEST") == "" {
		t.Skip("set AIC_BROWSER_TEST=1 for isolated Chrome")
	}
	previous := cfg.AuthSnapshot()
	defer cfg.SetAuth(previous)
	open := previous
	open.ExecPolicy = "open"
	open.ExecDeny = nil
	cfg.SetAuth(open)
	c, _ := testClient(t)
	caller := testCaller()
	invoke := func(method string, args any, execution *wire.ExecutionOptions, waitMS int64) wire.Response {
		raw, _ := json.Marshal(args)
		return c.HandleTool(context.Background(), caller, wire.Request{ID: wire.NewID("r_"), Action: "call", Call: &wire.Invocation{Domain: "exec", Command: "browser", Method: method, Args: raw}, Execution: execution, TimeoutMS: waitMS})
	}
	created := invoke("page.create", browser.CreateArgs{URL: "about:blank"}, nil, 30000)
	if created.Error != nil {
		t.Fatal(created.Error)
	}
	page := decoded[browser.PageInfo](t, created.Result)
	// 等待上限 50ms：page.wait 以执行记录立即返回（后台继续），取消不关共享页面。
	start := invoke("page.wait", browser.WaitArgs{PageID: page.ID, Text: "never appears", TimeoutMS: 30000}, &wire.ExecutionOptions{Epoch: c.procs.Epoch(), ID: "browser_wait"}, 50)
	if start.Error != nil {
		t.Fatal(start.Error)
	}
	run := decoded[exec_procs.Result](t, start.Result)
	if !run.Background || run.LogPath == "" {
		t.Fatal(run)
	}
	if _, err := c.executionControl(context.Background(), caller, "bg_kill", []string{run.ID}); err != nil {
		t.Fatal(err)
	}
	done, err := c.procs.Wait(context.Background(), run.ID, 2*time.Second)
	if err != nil || done.Status != "cancelled" || done.ProcessExit != nil {
		t.Fatal(done, err)
	}
	pages := invoke("page.list", browser.Empty{}, nil, 30000)
	if pages.Error != nil {
		t.Fatal(pages.Error)
	}
	list := decoded[[]browser.PageInfo](t, pages.Result)
	if len(list) != 1 || list[0].ID != page.ID {
		t.Fatal("cancelling wait closed shared page", list)
	}
}
