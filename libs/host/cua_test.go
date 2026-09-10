package host

// cua 测试：argv→MCP 装配（纯函数）+ 探测 + 真机集成（本机装了 cua-driver 才跑，
// 经 runCua 走完整链路：懒启动 → 握手 → tools/call → 应答整形）。

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/veypi/aic-pod/libs/proto"
)

func TestCuaMcpArgs(t *testing.T) {
	cases := []struct {
		name string
		goos string
		sock string
		want []string
	}{
		{"darwin daemon 唯一形态", "darwin", "/tmp/x.sock", []string{"mcp", "--socket", "/tmp/x.sock"}},
		{"linux 保留 direct 过渡", "linux", "", []string{"mcp", "--direct", "--grant", "existing-profile"}},
		{"windows 保留 direct 过渡", "windows", "", []string{"mcp", "--direct", "--grant", "existing-profile"}},
	}
	for _, c := range cases {
		if got := cuaMcpArgs(c.goos, c.sock); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

// daemon 自动拉起参数（纯函数）：空 appPath = 用户自装形态（-a CuaDriver）；
// 非空 = 桌面端内置 app 路径直启（open -n -g <app> --args serve --grant ...）。
func TestCuaDaemonLaunchArgs(t *testing.T) {
	cases := []struct {
		name    string
		appPath string
		want    []string
	}{
		{"用户自装（-a CuaDriver）", "", []string{"-n", "-g", "-a", "CuaDriver", "--args", "serve", "--grant", "existing-profile"}},
		{"内置 app 路径直启", "/app/Resources/cua/darwin/CuaDriver.app", []string{"-n", "-g", "/app/Resources/cua/darwin/CuaDriver.app", "--args", "serve", "--grant", "existing-profile"}},
	}
	for _, c := range cases {
		if got := cuaDaemonLaunchArgs(c.appPath); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: cuaDaemonLaunchArgs(%q) = %v, want %v", c.name, c.appPath, got, c.want)
		}
	}
}

func TestMapCuaArgv(t *testing.T) {
	type want struct {
		cli      bool
		tool     string
		args     map[string]any
		snapshot bool
		png      bool
		grep     string
		grepCtx  int
		bind     bool
		target   bool
	}
	cases := []struct {
		argv []string
		want want
	}{
		{[]string{"doctor"}, want{cli: true}},
		{[]string{"apps"}, want{tool: "list_apps", args: map[string]any{}}},
		{[]string{"windows"}, want{tool: "list_windows", args: map[string]any{}}},
		{[]string{"windows", "--pid", "1234"}, want{tool: "list_windows", args: map[string]any{"pid": 1234}}},
		{[]string{"snapshot", "--pid", "1234", "--window", "56"}, want{
			tool: "get_window_state", snapshot: true, args: map[string]any{"pid": 1234, "window_id": 56}}},
		{[]string{"snapshot", "--pid", "1234", "--window", "56", "--png"}, want{
			tool: "get_window_state", snapshot: true, png: true,
			args: map[string]any{"pid": 1234, "window_id": 56}}},
		{[]string{"snapshot", "--pid", "1234", "--window", "56", "--png", "--grep", "搜索"}, want{
			tool: "get_window_state", snapshot: true, png: true, grep: "搜索", grepCtx: 2,
			args: map[string]any{"pid": 1234, "window_id": 56}}},
		{[]string{"launch", "--app", "Notes"}, want{tool: "launch_app", args: map[string]any{"name": "Notes"}}},
		{[]string{"click", "--token", "t1", "--pid", "1", "--window", "2"}, want{
			tool: "click", args: map[string]any{"pid": 1, "window_id": 2, "element_token": "t1"}}},
		{[]string{"dclick", "--x", "100", "--y", "200"}, want{
			tool: "double_click", args: map[string]any{"x": 100.0, "y": 200.0}}},
		{[]string{"click", "--token", "t1", "--delivery", "foreground"}, want{
			tool: "click", args: map[string]any{"element_token": "t1", "delivery_mode": "foreground"}}},
		{[]string{"type", "--text", "hello world", "--token", "t9"}, want{
			tool: "type_text", args: map[string]any{"element_token": "t9", "text": "hello world"}}},
		{[]string{"key", "enter"}, want{tool: "press_key", args: map[string]any{"key": "enter"}}},
		{[]string{"hotkey", "cmd+shift+s"}, want{
			tool: "press_key", args: map[string]any{"key": "s", "modifiers": []string{"cmd", "shift"}}}},
		{[]string{"scroll", "--direction", "down", "--amount", "3", "--pid", "1"}, want{
			tool: "scroll", args: map[string]any{"pid": 1, "direction": "down", "amount": 3}}},
		{[]string{"drag", "--x1", "1", "--y1", "2", "--x2", "3", "--y2", "4"}, want{
			tool: "drag", args: map[string]any{"from_x": 1.0, "from_y": 2.0, "to_x": 3.0, "to_y": 4.0}}},
		{[]string{"move", "--x", "10", "--y", "20"}, want{tool: "move_cursor", args: map[string]any{"x": 10.0, "y": 20.0}}},
		{[]string{"set-value", "--token", "t1", "--value", "on", "--pid", "7"}, want{
			tool: "set_value", args: map[string]any{"element_token": "t1", "value": "on", "pid": 7}}},
		{[]string{"menu", "--pid", "1", "--path", "File>Save"}, want{
			tool: "invoke_menu", args: map[string]any{"path": []string{"File", "Save"}, "pid": 1}}},
		{[]string{"set-frame", "--pid", "1", "--window", "2", "--x", "0", "--y", "0", "--width", "800", "--height", "600"}, want{
			tool: "set_window_frame",
			args: map[string]any{"pid": 1, "window_id": 2, "x": 0.0, "y": 0.0, "width": 800.0, "height": 600.0}}},
		{[]string{"clipboard", "read"}, want{tool: "clipboard_read", args: map[string]any{"include_text": true}}},
		{[]string{"clipboard", "write", "hello"}, want{tool: "clipboard_write", args: map[string]any{"text": "hello"}}},
		// cursor：agent 光标浮层（on/off/state/motion/theme）
		{[]string{"cursor", "off"}, want{tool: "set_agent_cursor_enabled", args: map[string]any{"enabled": false}}},
		{[]string{"cursor", "on"}, want{tool: "set_agent_cursor_enabled", args: map[string]any{"enabled": true}}},
		{[]string{"cursor", "state"}, want{tool: "get_agent_cursor_state", args: map[string]any{}}},
		{[]string{"cursor", "motion", "--glide-duration-ms", "700", "--spring", "0.72"}, want{
			tool: "set_agent_cursor_motion",
			args: map[string]any{"glide_duration_ms": 700, "spring": 0.72}}},
		{[]string{"cursor", "theme", "cua.default"}, want{
			tool: "set_agent_cursor_theme", args: map[string]any{"theme_id": "cua.default"}}},
		{[]string{"cursor", "theme", "cua.default", "--reduced-motion", "off"}, want{
			tool: "set_agent_cursor_theme",
			args: map[string]any{"theme_id": "cua.default", "reduced_motion": "off"}}},
		{[]string{"cursor", "theme", "--theme-id", "cua.default"}, want{
			tool: "set_agent_cursor_theme", args: map[string]any{"theme_id": "cua.default"}}},
		// 未知 flag 透传：kebab→snake，值类型自动推断（int/float/bool/string）
		{[]string{"click", "--x", "10", "--y", "20", "--count", "2"}, want{
			tool: "click", args: map[string]any{"x": 10.0, "y": 20.0, "count": 2}}},
		{[]string{"click", "--x", "10", "--y", "20", "--debug-image-out", "/tmp/d.png"}, want{
			tool: "click", args: map[string]any{"x": 10.0, "y": 20.0, "debug_image_out": "/tmp/d.png"}}},
		{[]string{"click", "--x", "10", "--y", "20", "--from-zoom"}, want{
			tool: "click", args: map[string]any{"x": 10.0, "y": 20.0, "from_zoom": true}}},
		{[]string{"click", "--x", "10", "--y", "20", "--foo", "1.5"}, want{
			tool: "click", args: map[string]any{"x": 10.0, "y": 20.0, "foo": 1.5}}},
		{[]string{"move", "--x", "10", "--y", "20", "--scope", "desktop"}, want{
			tool: "move_cursor", args: map[string]any{"x": 10.0, "y": 20.0, "scope": "desktop"}}},
		// run 为本地特化（tool 空），未知 flag 不透传
		{[]string{"run", "--code", "return 1", "--bogus", "1"}, want{}},
		// launch --url：多 URL 全量收集
		{[]string{"launch", "--app", "Google Chrome", "--url", "https://a.com", "--url", "https://b.com"}, want{
			tool: "launch_app", args: map[string]any{"name": "Google Chrome", "urls": []string{"https://a.com", "https://b.com"}}}},
		// snapshot --grep/--context
		{[]string{"snapshot", "--pid", "1", "--window", "2", "--grep", "搜索"}, want{
			tool: "get_window_state", snapshot: true, args: map[string]any{"pid": 1, "window_id": 2}, grep: "搜索", grepCtx: 2}},
		{[]string{"snapshot", "--pid", "1", "--window", "2", "--grep", "地址", "--context", "5"}, want{
			tool: "get_window_state", snapshot: true, args: map[string]any{"pid": 1, "window_id": 2}, grep: "地址", grepCtx: 5}},
		// typed browser 家族
		{[]string{"browser-state", "--pid", "13882", "--window", "41239"}, want{
			tool: "get_browser_state", bind: true,
			args: map[string]any{"pid": 13882, "window_id": 41239, "snapshot_format": "semantic_v2"}}},
		{[]string{"browser-state", "--pid", "1", "--query", "input"}, want{
			tool: "get_browser_state", bind: true,
			args: map[string]any{"pid": 1, "snapshot_format": "semantic_v2", "query": "input"}}},
		{[]string{"navigate", "--url", "https://example.com"}, want{
			tool: "browser_navigate", target: true, args: map[string]any{"url": "https://example.com"}}},
		{[]string{"navigate", "https://example.com"}, want{
			tool: "browser_navigate", target: true, args: map[string]any{"url": "https://example.com"}}},
		{[]string{"bclick", "--ref", "e12"}, want{
			tool: "browser_click", target: true, args: map[string]any{"ref": "e12"}}},
		{[]string{"bclick", "--x", "100", "--y", "200", "--route", "dom_event"}, want{
			tool: "browser_click", target: true, args: map[string]any{"x": 100.0, "y": 200.0, "input_route": "dom_event"}}},
		{[]string{"btype", "--ref", "e5", "--text", "gpt6"}, want{
			tool: "browser_type", target: true, args: map[string]any{"ref": "e5", "text": "gpt6"}}},
		{[]string{"btype", "--ref", "e5", "--text", "x", "--mode", "keystrokes", "--replace"}, want{
			tool: "browser_type", target: true,
			args: map[string]any{"ref": "e5", "text": "x", "mode": "keystrokes", "replace": true}}},
		{[]string{"bprepare", "--isolated"}, want{
			tool: "browser_prepare", bind: true,
			args: map[string]any{"profile": map[string]any{"mode": "isolated_new"}, "allow_launch": true}}},
		{[]string{"bprepare", "--pid", "13882", "--window", "41239"}, want{
			tool: "browser_prepare", bind: true,
			args: map[string]any{"pid": 13882, "window_id": 41239, "strategy": map[string]any{"kind": "existing_profile"}}}},
		{[]string{"bend"}, want{tool: "end_session", args: map[string]any{}}},
		{[]string{"browser-state", "--target", "t1", "--tab", "b2"}, want{
			tool: "get_browser_state", bind: true,
			args: map[string]any{"target_id": "t1", "tab_id": "b2", "snapshot_format": "semantic_v2"}}},
	}
	for _, c := range cases {
		got, err := mapCuaArgv(c.argv)
		if err != nil {
			t.Errorf("mapCuaArgv(%v) error: %v", c.argv, err)
			continue
		}
		if got.cli != c.want.cli || got.tool != c.want.tool || got.snapshot != c.want.snapshot ||
			got.png != c.want.png ||
			got.grep != c.want.grep || got.grepCtx != c.want.grepCtx ||
			got.browserBind != c.want.bind || got.browserTarget != c.want.target {
			t.Errorf("mapCuaArgv(%v) = %+v, want %+v", c.argv, got, c.want)
			continue
		}
		if len(got.args) != len(c.want.args) {
			t.Errorf("mapCuaArgv(%v).args = %v, want %v", c.argv, got.args, c.want.args)
			continue
		}
		for k, v := range c.want.args {
			if !cuaArgsEqual(got.args[k], v) {
				t.Errorf("mapCuaArgv(%v).args[%s] = %v (%T), want %v (%T)", c.argv, k, got.args[k], got.args[k], v, v)
			}
		}
	}
}

func cuaArgsEqual(a, b any) bool {
	switch av := a.(type) {
	case []string:
		bv, ok := b.([]string)
		if !ok || len(av) != len(bv) {
			return false
		}
		for i := range av {
			if av[i] != bv[i] {
				return false
			}
		}
		return true
	case map[string]any:
		return reflect.DeepEqual(av, b)
	}
	return a == b
}

func TestMapCuaArgvErrors(t *testing.T) {
	cases := [][]string{
		{"explode"},               // 未知子命令
		{},                        // 空
		{"click", "--x", "abc"}, // 非数字
		{"snapshot", "--pid", "1234"},  // 缺 --window
		{"drag", "--x1", "1"},          // 缺必填
		{"set-value", "--value", "on"}, // 缺 --token
		{"set-value", "--token", "t1"}, // 缺 --value
		{"clipboard"},                  // 缺 read|write
		{"clipboard", "write"},         // 缺文本
		{"cursor"},                     // 缺嵌套子命令
		{"cursor", "bogus"},            // 未知嵌套子命令
		{"cursor", "theme"},            // 缺 theme_id
		{"type"},                       // 缺 --text
		{"menu", "--pid", "1"},         // 缺 --path
		{"front"},                      // 缺 --pid
		{"front", "--window", "56"},    // 缺 --pid
		{"click", "--pid"},             // flag 缺值
		{"navigate"},                   // 缺 --url
		{"bclick"},                     // 缺 --ref/--x
		{"btype", "--text", "x"},       // 缺 --ref
		{"btype", "--ref", "e1"},       // 缺 --text
		{"bprepare"},                   // existing_profile 缺 --pid
		{"bprepare", "--pid", "1"}, // existing_profile 缺 --window
		{"run"}, // 缺 --code/--file
		{"run", "--code", "return 1", "--file", "/tmp/x.js"}, // 二选一互斥
	}
	for _, c := range cases {
		if _, err := mapCuaArgv(c); err == nil {
			t.Errorf("mapCuaArgv(%v) should error", c)
		}
	}
}

// run 分支：--code/--file 二选一必填。
func TestMapCuaArgvRun(t *testing.T) {
	c, err := mapCuaArgv([]string{"run", "--code", "return 1"})
	if err != nil || !c.script || c.code != "return 1" || c.file != "" {
		t.Fatalf("run --code = %+v %v", c, err)
	}
	c, err = mapCuaArgv([]string{"run", "--file", "/tmp/a.js"})
	if err != nil || !c.script || c.file != "/tmp/a.js" || c.code != "" {
		t.Fatalf("run --file = %+v %v", c, err)
	}
	// 位置参数作 file 简写
	c, err = mapCuaArgv([]string{"run", "/tmp/b.js"})
	if err != nil || c.file != "/tmp/b.js" {
		t.Fatalf("run <file> = %+v %v", c, err)
	}
}

// 真机集成：本机装了 cua-driver 才跑（CI 无此二进制时跳过）。
func TestCuaLive(t *testing.T) {
	if findCuaDriver() == "" {
		t.Skip("cua-driver not installed")
	}
	// 会话工作区重定向到测试临时目录（须位于 $HOME/.aic/sessions/ 布局外也合法——
	// Go 原生实现无壳侧 jail，sessionWorkDir 直接计算；测试用 cfg 回落路径）。
	initCuaRuntime(t.Logf)
	if cuaRt == nil {
		t.Skip("cua runtime not initialized")
	}
	cl := &Client{}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	req := &proto.ToolRequest{MsgID: "cua-live-test"}

	// apps：感知读
	r := cl.runCua(ctx, "cua-live", req, []string{"apps"})
	if r.State != proto.StateCompleted || !strings.Contains(r.Content, "app(s)") {
		t.Fatalf("apps = %s %s %s", r.State, r.Error, r.Content)
	}
	// clipboard 往返（include_text 修正）
	marker := "cua-live-marker-x7"
	r = cl.runCua(ctx, "cua-live", req, []string{"clipboard", "write", marker})
	if r.State != proto.StateCompleted {
		t.Fatalf("clipboard write = %s %s", r.State, r.Error)
	}
	r = cl.runCua(ctx, "cua-live", req, []string{"clipboard", "read"})
	if r.State != proto.StateCompleted || !strings.Contains(r.Content, marker) {
		t.Fatalf("clipboard read = %s %s %s", r.State, r.Error, r.Content)
	}
	// windows：structuredContent 带出
	r = cl.runCua(ctx, "cua-live", req, []string{"windows"})
	if r.State != proto.StateCompleted || !strings.Contains(r.Content, `"window_id"`) {
		t.Fatalf("windows = %s %s %s", r.State, r.Error, r.Content[:min(200, len(r.Content))])
	}
	// doctor：一次性 CLI
	r = cl.runCua(ctx, "cua-live", req, []string{"doctor"})
	if r.State != proto.StateCompleted || !strings.Contains(r.Content, "cua-driver") {
		t.Fatalf("doctor = %s %s", r.State, r.Error)
	}
	// 未知子命令报错暴露
	r = cl.runCua(ctx, "cua-live", req, []string{"explode"})
	if r.State != proto.StateError || !strings.Contains(r.Error, "unknown cua subcommand") {
		t.Fatalf("explode = %s %s", r.State, r.Error)
	}
}

// 产物落盘集成：snapshot --png（找一个真实窗口；无窗口环境跳过）。
func TestCuaLiveSnapshotPng(t *testing.T) {
	if findCuaDriver() == "" {
		t.Skip("cua-driver not installed")
	}
	initCuaRuntime(t.Logf)
	if cuaRt == nil {
		t.Skip("cua runtime not initialized")
	}
	cl := &Client{}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	req := &proto.ToolRequest{MsgID: "cua-live-png"}

	// 窗口截图路径（窗口态依赖真实窗口；选一个 on-screen 窗口，找不到则跳过）。
	// 用结构化 list_windows 选窗口：文本粗解析会命中离屏/辅助窗口（不可截图）。
	wres, werr := cuaRt.call(ctx, "list_windows", map[string]any{})
	if werr != nil {
		t.Skip("no window list: " + werr.Error())
	}
	pid, wid := firstOnScreenWindow(wres.StructuredContent)
	if pid == 0 {
		t.Skip("no on-screen window found")
	}
	// 默认（不带 --png）：只出正文（txt 落盘），不出图
	r := cl.runCua(ctx, "cua-live-png", req, []string{
		"snapshot", "--pid", itoa(pid), "--window", itoa(wid)})
	if r.State != proto.StateCompleted {
		t.Fatalf("snapshot = %s %s", r.State, r.Error)
	}
	if r.Attrs["path"] != "" || strings.Contains(r.Content, "[image]") {
		t.Fatalf("snapshot default should not produce image, path=%q content=%s",
			r.Attrs["path"], r.Content[:min(200, len(r.Content))])
	}
	if !strings.Contains(r.Content, "[text]") {
		t.Fatalf("snapshot default missing [text]: %s", r.Content[:min(300, len(r.Content))])
	}
	// --png：出图落盘 + image_data 附返回
	r = cl.runCua(ctx, "cua-live-png", req, []string{
		"snapshot", "--pid", itoa(pid), "--window", itoa(wid), "--png"})
	if r.State != proto.StateCompleted {
		t.Fatalf("snapshot --png = %s %s", r.State, r.Error)
	}
	if r.Attrs["path"] == "" {
		t.Fatalf("snapshot --png missing attrs.path, content=%s", r.Content[:min(200, len(r.Content))])
	}
	// 服务端会把大图压成 JPEG 再投喂模型（image_compressed 记录原始尺寸）；
	// 断言只校验 data URI 形态，不锁死 mime。
	imgData := r.Attrs["image_data"]
	if !strings.HasPrefix(imgData, "data:image/png;base64,") && !strings.HasPrefix(imgData, "data:image/jpeg;base64,") {
		t.Fatalf("snapshot --png missing image_data, attrs=%v", r.Attrs)
	}
	fi, err := os.Stat(r.Attrs["path"])
	if err != nil || fi.Size() == 0 {
		t.Fatalf("png file = %v %v", fi, err)
	}
	// PNG magic 校验
	f, _ := os.Open(r.Attrs["path"])
	defer f.Close()
	head := make([]byte, 2)
	f.Read(head)
	if head[0] != 0x89 || head[1] != 0x50 {
		t.Fatalf("not a png: %v", head)
	}
	if !strings.Contains(r.Attrs["path"], filepath.Join(".cua", "")) {
		t.Fatalf("png not in .cua dir: %s", r.Attrs["path"])
	}
	// 正文成对落盘：同名 .txt 存在且非空（tree_markdown/elements 全量）
	txtPath := strings.TrimSuffix(r.Attrs["path"], ".png") + ".txt"
	if fi, err := os.Stat(txtPath); err != nil || fi.Size() == 0 {
		t.Fatalf("snapshot txt missing or empty: %s (%v %v)", txtPath, fi, err)
	}
	// 返回精简：应含摘要头 + [text] 路径，且不超 8KB（不含全量树正文）
	if !strings.Contains(r.Content, "cua snapshot:") || !strings.Contains(r.Content, "[text]") {
		t.Fatalf("snapshot content missing summary or [text]: %s", r.Content[:min(300, len(r.Content))])
	}
	if len(r.Content) > 8*1024 {
		t.Fatalf("snapshot content should be slim, got %d bytes", len(r.Content))
	}
}

// firstOnScreenWindow 从 list_windows structuredContent 选第一个 on-screen 窗口，
// 跳过 cua-driver 自身进程（驱动拒绝操作自己的授权进程）。
func firstOnScreenWindow(sc map[string]any) (pid, wid int) {
	wins, _ := sc["windows"].([]any)
	for _, item := range wins {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if on, _ := m["is_on_screen"].(bool); !on {
			continue
		}
		if name, _ := m["app_name"].(string); strings.Contains(name, "Cua Driver") {
			continue
		}
		p, _ := m["pid"].(float64)
		wv, _ := m["window_id"].(float64)
		if p != 0 && wv != 0 {
			return int(p), int(wv)
		}
	}
	return 0, 0
}

func itoa(n int) string {
	return strconv.Itoa(n)
}

// grepTree 过滤语义：命中行 + 祖先缩进链 + 前后 context 行 + 命中元素索引。
func TestGrepTree(t *testing.T) {
	tree := "window_id=100 pid=200 size=800x600 elements=7\n" +
		"\n" +
		"- [0] AXWindow \"主窗口\" [actions=[raise]]\n" +
		"    - [1] AXToolbar\n" +
		"      - [2] AXButton (返回) [actions=[press]]\n" +
		"      - [3] AXTextField = \"ivec.ai\" (地址和搜索栏) [actions=[press]]\n" +
		"  - [4] AXWebArea\n" +
		"    - [5] AXStaticText = \"无关内容\"\n" +
		"    - [6] AXButton (搜索) [actions=[press]]\n"

	// 命中+祖先链+context：搜 "地址" 应保留头部、AXWindow、AXToolbar、命中行与前后行
	out, idxs := grepTree(tree, "地址", 1)
	if len(idxs) != 1 || idxs[0] != 3 {
		t.Fatalf("idxs = %v", idxs)
	}
	for _, want := range []string{"window_id=100", "AXWindow", "AXToolbar", "地址和搜索栏", "[grep \"地址\": 1 命中"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
	// context=1：命中行 5（0-based 全行号）±1 保留 [2]/[4]，[5]/[6] 行应被省略
	if strings.Contains(out, "AXButton (返回)") == false { // [2] 是命中前一行，应保留
		t.Fatalf("context line missing:\n%s", out)
	}
	if strings.Contains(out, "无关内容") {
		t.Fatalf("unrelated line leaked:\n%s", out)
	}
	// 省略段标记存在（AXWindow 与 Toolbar 之间的断链）
	if !strings.Contains(out, "…") {
		t.Fatalf("ellipsis missing:\n%s", out)
	}

	// 多命中：搜 "AXButton" 应得 2 个索引
	_, idxs2 := grepTree(tree, "AXButton", 0)
	if len(idxs2) != 2 {
		t.Fatalf("idxs2 = %v", idxs2)
	}

	// 大小写不敏感
	if _, idxs3 := grepTree(tree, "axbutton", 0); len(idxs3) != 2 {
		t.Fatalf("case-insensitive failed: %v", idxs3)
	}

	// 零命中返回空
	if out4, idxs4 := grepTree(tree, "不存在", 2); out4 != "" || idxs4 != nil {
		t.Fatalf("no-match = %q %v", out4, idxs4)
	}
}

// cuaSnapshotSummary 语义提炼：无 [N] 索引的树行（说明文本/菜单项）也要进摘要。
func TestCuaSnapshotSummary(t *testing.T) {
	tree := "window_id=1 pid=2 size=3x3 elements=5\n" +
		"- [0] AXWindow \"主窗口\" [actions=[raise]]\n" +
		"  - [1] AXButton (确认) [actions=[press]]\n" +
		"- [2] AXMenuBarItem \"文件\" [actions=[cancel,press,pick]]\n" +
		"    - AXStaticText = \"说明文字\"\n" + // 无索引行：elements 数组不含
		"- AXMenuItem \"保存\" [id=x actions=[press]]\n"
	els := []map[string]any{
		{"element_index": 0.0, "element_token": "s1:0", "role": "AXWindow", "label": "主窗口", "depth": 0.0},
		{"element_index": 1.0, "element_token": "s1:1", "role": "AXButton", "label": "确认", "depth": 1.0,
			"frame": map[string]any{"x": 1.0, "y": 2.0, "w": 3.0, "h": 4.0}},
		{"element_index": 2.0, "element_token": "s1:2", "role": "AXMenuBarItem", "label": "文件", "depth": 1.0},
	}
	out := cuaSnapshotSummary(els, tree)
	for _, want := range []string{"说明文字", "确认", "s1:1", "菜单项", "AXWindow", "文件"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in summary:\n%s", want, out)
		}
	}
}

// Blender 类 OpenGL viewport app 的能力探针：默认跳过。
//
// 驱动 0.24.0 对这类 app **没有后台事件路径**（驱动 skill MACOS.md
// § Canvases, viewports, games 明确点名 Blender/Unity/GHOST/Qt/wxWidgets）：
// per-pid 事件路径（SLEventPostToPid / CGEvent.postToPid）会被 viewport 自己的
// event-source 检查过滤（要求 real HID origin），键盘仅有 post_to_pid 路径
// （auth-message envelope 面向 Chromium/Electron）。实测：全局单键（F3）可送达，
// 组合键（shift+a，修饰符丢失变裸 a）、type_text（AX 路径）、像素 click 均无效。
// 官方唯一路径是“持久前台 + cghidEventTap”（会抢焦点/移动真实光标），与
// “mac 全后台”决策冲突。驱动补上“合成焦点 + HID tap”后再启用本测试。
// 手动跑：CUA_LIVE_BLENDER=1 go test ./libs/host/ -run TestCuaLiveBlender。
func TestCuaLiveBlender(t *testing.T) {
	if os.Getenv("CUA_LIVE_BLENDER") != "1" {
		t.Skip("Blender has no backgrounded path in cua-driver (see MACOS.md § viewports)")
	}
	if findCuaDriver() == "" {
		t.Skip("cua-driver not installed")
	}
	initCuaRuntime(t.Logf)
	if cuaRt == nil {
		t.Skip("cua runtime not initialized")
	}
	cl := &Client{}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	req := &proto.ToolRequest{MsgID: "cua-live-blender"}

	wres, werr := cuaRt.call(ctx, "list_windows", map[string]any{})
	if werr != nil {
		t.Skip("no window list: " + werr.Error())
	}
	pid, wid, _, _ := blenderMainWindow(wres.StructuredContent)
	if pid == 0 {
		t.Skip("no on-screen Blender window")
	}
	// 能力探针：snapshot 读可用（AX 树稀疏但窗口可截）；写操作受驱动限制。
	r := cl.runCua(ctx, "cua-live-blender", req, []string{
		"snapshot", "--pid", itoa(pid), "--window", itoa(wid)})
	if r.State != proto.StateCompleted {
		t.Fatalf("blender snapshot = %s %s", r.State, r.Error)
	}
}

// blenderMainWindow 从 list_windows structuredContent 选在屏的 Blender 主窗口
// （title 非空），返回 pid/window 与窗口中心像素坐标。
func blenderMainWindow(sc map[string]any) (pid, wid, cx, cy int) {
	wins, _ := sc["windows"].([]any)
	for _, item := range wins {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		name, _ := m["app_name"].(string)
		title, _ := m["title"].(string)
		if !strings.Contains(name, "Blender") || title == "" {
			continue
		}
		if on, _ := m["is_on_screen"].(bool); !on {
			continue
		}
		p, _ := m["pid"].(float64)
		wv, _ := m["window_id"].(float64)
		b, _ := m["bounds"].(map[string]any)
		if p == 0 || wv == 0 || b == nil {
			continue
		}
		bx, _ := b["x"].(float64)
		by, _ := b["y"].(float64)
		bw, _ := b["width"].(float64)
		bh, _ := b["height"].(float64)
		return int(p), int(wv), int(bx + bw/2), int(by + bh/2)
	}
	return 0, 0, 0, 0
}

// typed browser 家族真机集成：bprepare --isolated 起驱动自持隔离浏览器 →
// navigate 打开新页。默认跳过：驱动的隔离浏览器每次新建临时 profile，首启会弹
// 欢迎/默认浏览器提示，需人工点掉才就绪（不点即超时）；existing_profile 又被驱动
// 硬编码英文 "New Tab" 挡住（中文界面浏览器必拒，platform-macos/browser/setup_ui.rs）。
// 驱动修好后去掉此门。手动跑：CUA_LIVE_BROWSER=1 go test ./libs/host/ -run TestCuaLiveBrowser。
func TestCuaLiveBrowser(t *testing.T) {
	if os.Getenv("CUA_LIVE_BROWSER") != "1" {
		t.Skip("browser E2E skipped: driver isolated-profile welcome prompt + en-US 'New Tab' assumption")
	}
	if findCuaDriver() == "" {
		t.Skip("cua-driver not installed")
	}
	initCuaRuntime(t.Logf)
	cl := &Client{}
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	req := &proto.ToolRequest{MsgID: "cua-live-browser"}
	// daemon 模式下 session 状态常驻驱动进程：固定 sid 会被上轮的 bend 永久
	// 终结，每次跑用唯一 sid。
	sid := "cua-live-browser-" + strconv.FormatInt(time.Now().UnixNano(), 36)

	// navigate 未绑定时应报引导错误
	cuaRt.setBrowserBinding("", "")
	r := cl.runCua(ctx, sid, req, []string{"navigate", "--url", "https://example.com"})
	if r.State != proto.StateError || !strings.Contains(r.Error, "browser-state") {
		t.Fatalf("navigate without binding = %s %s", r.State, r.Error)
	}
	// bprepare --isolated：驱动自建隔离 profile + 首个 tab（应答无 CDP 绑定，
	// attachment=null）。窗口/页面就绪是异步的：首启可能 30s+（批量测试负载下
	// 更慢），驱动会复用已启动的隔离浏览器——首轮超时后重来一轮通常秒过。
	var tid, tab string
	for attempt := 1; attempt <= 2 && (tid == "" || tab == ""); attempt++ {
		r = cl.runCua(ctx, sid, req, []string{"bprepare", "--isolated"})
		if r.State != proto.StateCompleted {
			t.Fatalf("bprepare = %s %s %s", r.State, r.Error, r.Content[:min(400, len(r.Content))])
		}
		tid, tab = cuaRt.browserBinding()
		deadline := time.Now().Add(45 * time.Second)
		for tid == "" || tab == "" {
			r = cl.runCua(ctx, sid, req, []string{"browser-state"})
			if r.State == proto.StateCompleted {
				tid, tab = cuaRt.browserBinding()
				if tid != "" && tab != "" {
					break
				}
			}
			if time.Now().After(deadline) {
				break
			}
			time.Sleep(500 * time.Millisecond)
		}
	}
	if tid == "" || tab == "" {
		t.Fatalf("binding empty after bprepare, state=%s err=%s content=%s",
			r.State, r.Error, r.Content[:min(500, len(r.Content))])
	}
	// navigate 到 example.com
	r = cl.runCua(ctx, sid, req, []string{
		"navigate", "--url", "https://example.com/"})
	if r.State != proto.StateCompleted {
		t.Fatalf("navigate = %s %s", r.State, r.Error)
	}
	// bend：回收隔离浏览器会话
	r = cl.runCua(ctx, sid, req, []string{"bend"})
	if r.State != proto.StateCompleted {
		t.Fatalf("bend = %s %s", r.State, r.Error)
	}
}

// cua run 真机集成：本机有 cua-driver + node 才跑（CI 缺任一则跳过）。
// 脚本经桥做 clipboard 往返 + log + return 值，验证全链路（桥/runner/transcript）。
func TestCuaLiveRun(t *testing.T) {
	if findCuaDriver() == "" {
		t.Skip("cua-driver not installed")
	}
	if _, _, err := findNodeBin(); err != nil {
		t.Skip("node runtime not found")
	}
	initCuaRuntime(t.Logf)
	if cuaRt == nil {
		t.Skip("cua runtime not initialized")
	}
	cl := &Client{}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	req := &proto.ToolRequest{MsgID: "cua-live-run"}

	marker := "cua-run-marker-42"
	script := `
await cua.clipboardWrite("` + marker + `")
const clip = await cua.clipboardRead()
if (!JSON.stringify(clip).includes("` + marker + `")) {
  throw new Error("clipboard mismatch")
}
await cua.sleep(10)
cua.log("roundtrip ok")
return { seen: true }
`
	r := cl.runCua(ctx, "cua-live-run", req, []string{"run", "--code", script})
	if r.State != proto.StateCompleted {
		t.Fatalf("run = %s %s\n%s", r.State, r.Error, r.Content)
	}
	if !strings.Contains(r.Content, "cua run: OK") || !strings.Contains(r.Content, "clipboard.write") {
		t.Fatalf("transcript missing steps: %s", r.Content)
	}
	if !strings.Contains(r.Content, `"seen":true`) {
		t.Fatalf("transcript missing result: %s", r.Content)
	}
	if r.Attrs["path"] == "" {
		t.Fatalf("run missing log path attr")
	}

	// 脚本未捕获异常 → StateError，已执行步骤随 transcript 带回
	r = cl.runCua(ctx, "cua-live-run", req, []string{"run", "--code",
		`await cua.clipboardWrite("x")
throw new Error("boom")`})
	if r.State != proto.StateError || !strings.Contains(r.Error, "boom") {
		t.Fatalf("failing script = %s %s", r.State, r.Error)
	}
	if !strings.Contains(r.Content, "[1] clipboard.write") || !strings.Contains(r.Content, "FAILED") {
		t.Fatalf("error transcript missing executed steps: %s", r.Content)
	}
}
