package host

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/veypi/aic-pod/cfg"
	"github.com/veypi/aic-pod/libs/execution"
	"github.com/veypi/aic-pod/protocol"
	"github.com/veypi/vsh/commands"
)

func TestCommandHelperProcess(t *testing.T) {
	switch os.Getenv("AIC_COMMAND_HELPER") {
	case "":
		return
	case "process":
		data, _ := io.ReadAll(os.Stdin)
		fmt.Fprintf(os.Stdout, "args=%s stdin=%s", strings.Join(os.Args[1:], ","), data)
		code := 0
		_, _ = fmt.Sscanf(os.Getenv("AIC_COMMAND_HELPER_EXIT"), "%d", &code)
		os.Exit(code)
	case "mcp":
		server := mcp.NewServer(&mcp.Implementation{Name: "command-test", Version: "1"}, nil)
		mcp.AddTool(server, &mcp.Tool{Name: "ping", Description: "return pong"}, func(_ context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "pong"}}}, nil, nil
		})
		if err := server.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
			os.Exit(2)
		}
		return
	case "stream":
		listener, err := net.Listen("tcp", os.Getenv("AIC_TEST_STREAM_ADDR"))
		if err != nil {
			os.Exit(2)
		}
		defer listener.Close()
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				_, _ = io.Copy(conn, conn)
			}()
		}
	}
}

func runHostCommandManagement(t *testing.T, c *Client, args ...string) (string, error) {
	t.Helper()
	var out, errOut bytes.Buffer
	inv := &commands.Invocation{Args: args, Stdout: &out, Stderr: &errOut, Env: map[string]string{}}
	err := c.cmdHostCommands(context.Background(), inv)
	if err != nil {
		t.Logf("commands %v: stdout=%q stderr=%q", args, out.String(), errOut.String())
	}
	return out.String(), err
}

func TestUnifiedProcessCommand(t *testing.T) {
	c, _ := testClient(t)
	_, err := runHostCommandManagement(t, c, "add", "hello",
		"--env", "AIC_COMMAND_HELPER=process", "--env", "AIC_COMMAND_HELPER_EXIT=7",
		"--", os.Args[0], "-test.run=^TestCommandHelperProcess$")
	if err != nil {
		t.Fatal(err)
	}
	req := execRequest("hello alpha beta", 30000)
	req.Exec.Stdin = "input-bytes"
	response := callTool(t, c, context.Background(), testCaller(), req)
	if response.Error != nil {
		t.Fatalf("exec error: %+v", response.Error)
	}
	result := decoded[protocol.Output](t, response.Result)
	if result.Attrs["exit_code"] != "7" {
		t.Fatalf("exit code: %+v", result.Attrs)
	}
	if !strings.Contains(result.Content, "stdin=input-bytes") || !strings.Contains(result.Content, "alpha") {
		t.Fatalf("process output = %q", result.Content)
	}
}

func TestUnifiedMCPCommand(t *testing.T) {
	c, _ := testClient(t)
	_, err := runHostCommandManagement(t, c, "add", "mcp-test", "--protocol", "mcp",
		"--env", "AIC_COMMAND_HELPER=mcp", "--", os.Args[0], "-test.run=^TestCommandHelperProcess$")
	if err != nil {
		t.Fatal(err)
	}
	response := callTool(t, c, context.Background(), testCaller(), execRequest("mcp-test ping", 30000))
	if response.Error != nil {
		t.Fatalf("exec error: %+v", response.Error)
	}
	result := decoded[protocol.Output](t, response.Result)
	if !strings.Contains(result.Content, "pong") {
		t.Fatalf("MCP output = %q", result.Content)
	}
}

func writeTestPackage(t *testing.T, root, skillID, program, addressEnv string) {
	t.Helper()
	dir := filepath.Join(root, skillID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := fmt.Sprintf(`{"schema":1,"commands":{"shared":{"run":[%q]}},"streams":{"view":{"command":"shared","address_env":%q}}}`, program, addressEnv)
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestUnifiedCommandConfigSurface(t *testing.T) {
	c, _ := testClient(t)
	root := t.TempDir()
	c.commandPackages = root
	writeTestPackage(t, root, "package-a", "bin/a", "A_STREAM_ADDR")
	if err := c.saveCommandPackageState(commandPackageState{NextOrder: 2, Packages: map[string]commandPackageRecord{
		"package-a": {SkillID: "package-a", Order: 1},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := c.cmds.reload(); err != nil {
		t.Fatal(err)
	}

	// 应用配置：AI 可直接写（不需审批）。
	if _, err := runHostCommandManagement(t, c, "config", "shared", "AGENT_BROWSER_EXECUTABLE_PATH=/bin/chrome"); err != nil {
		t.Fatal(err)
	}
	def, ok := c.cmds.definition("shared")
	if !ok {
		t.Fatal("command not registered")
	}
	if got := def.environment()["AGENT_BROWSER_EXECUTABLE_PATH"]; got != "/bin/chrome" {
		t.Fatalf("env not applied: %q", got)
	}
	if def.noSandbox() {
		t.Fatal("no_sandbox leaked from env write")
	}

	// 提权项：平台来源且无审批 → 拒绝。
	platformCtx := execution.WithOwner(context.Background(), "u:admin")
	if err := runHostCommandConfig(t, c, platformCtx, "config", "shared", "--no-sandbox"); err == nil {
		t.Fatal("no_sandbox was accepted without platform approval")
	}
	if def, _ := c.cmds.definition("shared"); def.noSandbox() {
		t.Fatal("no_sandbox applied despite denial")
	}

	// 平台来源且已审批 → 放行。
	approvedCtx := execution.WithGrantApproved(execution.WithOwner(context.Background(), "u:admin"), true)
	if err := runHostCommandConfig(t, c, approvedCtx, "config", "shared", "--no-sandbox"); err != nil {
		t.Fatal(err)
	}
	if def, _ := c.cmds.definition("shared"); !def.noSandbox() {
		t.Fatal("no_sandbox not applied after approval")
	}

	// 本机 owner（无平台归属）→ 免审批。
	if err := runHostCommandConfig(t, c, context.Background(), "config", "shared", "--sandbox"); err != nil {
		t.Fatal(err)
	}
	if def, _ := c.cmds.definition("shared"); def.noSandbox() {
		t.Fatal("no_sandbox not cleared")
	}

	// 删除单个 env 与整体清空。
	if err := runHostCommandConfig(t, c, context.Background(), "config", "shared", "--unset", "AGENT_BROWSER_EXECUTABLE_PATH"); err != nil {
		t.Fatal(err)
	}
	if def, _ := c.cmds.definition("shared"); len(def.environment()) != 1 {
		t.Fatalf("env not cleared: %+v", def.environment())
	}
	if err := runHostCommandConfig(t, c, context.Background(), "config", "shared", "--clear"); err != nil {
		t.Fatal(err)
	}
	if _, exists := c.options().CommandConfig["skill:package-a:shared"]; exists {
		t.Fatal("cleared config still present")
	}
}

// runHostCommandConfig 以指定 ctx 执行 commands 子命令（审批事实在 ctx 里）。
func runHostCommandConfig(t *testing.T, c *Client, ctx context.Context, args ...string) error {
	t.Helper()
	var out, errOut bytes.Buffer
	inv := &commands.Invocation{Args: args, Stdout: &out, Stderr: &errOut, Env: map[string]string{}}
	err := c.cmdHostCommands(ctx, inv)
	if err != nil {
		t.Logf("commands %v: stdout=%q stderr=%q", args, out.String(), errOut.String())
	}
	return err
}

func TestUnifiedSameNameDefinitionIsolation(t *testing.T) {
	c, _ := testClient(t)
	root := t.TempDir()
	c.commandPackages = root
	writeTestPackage(t, root, "package-a", "bin/a", "A_STREAM_ADDR")
	writeTestPackage(t, root, "package-b", "bin/b", "B_STREAM_ADDR")
	if err := c.saveCommandPackageState(commandPackageState{NextOrder: 3, Packages: map[string]commandPackageRecord{
		"package-a": {SkillID: "package-a", Order: 1},
		"package-b": {SkillID: "package-b", Order: 2},
	}}); err != nil {
		t.Fatal(err)
	}
	configs := map[string]cfg.CommandDefConfig{
		"skill:package-a:shared": {NoSandbox: true},
		"skill:package-b:shared": {NoSandbox: false},
	}
	fileCfg, err := cfg.LoadFile()
	if err != nil {
		t.Fatal(err)
	}
	fileCfg.CommandConfig = configs
	if err := cfg.Save(fileCfg); err != nil {
		t.Fatal(err)
	}
	c.setCommandOptions(nil, configs)
	if err := c.cmds.reload(); err != nil {
		t.Fatal(err)
	}
	def, ok := c.cmds.definition("shared")
	if !ok || def.ID != "skill:package-b:shared" {
		t.Fatalf("active definition = %+v", def)
	}
	if def.noSandbox() || def.Config.Run[0] != filepath.Join(root, "package-b", "bin", "b") {
		t.Fatalf("B inherited A configuration: %+v", def)
	}
	// 流与命令同命名空间：地址 = 别名 + 流名；同名命令的流随生效定义走。
	if _, ok := c.cmds.streamResolver("shared:view"); !ok {
		t.Fatal("active stream is not routable")
	}
	if err := c.uninstallCommandPackage("package-b"); err != nil {
		t.Fatal(err)
	}
	if _, exists := c.options().CommandConfig["skill:package-b:shared"]; exists {
		t.Fatal("uninstall retained the removed package owner config")
	}
	def, ok = c.cmds.definition("shared")
	if !ok || def.ID != "skill:package-a:shared" || !def.noSandbox() {
		t.Fatalf("A did not recover its own definition: %+v", def)
	}
	if _, ok := c.cmds.streamResolver("shared:view"); !ok {
		t.Fatal("stream did not recover with A")
	}
}

// skillPackageTreeJSON 把 {相对路径: 内容} 渲染成 /fs/cloud 目录列表 JSON。
func skillPackageTreeJSON(files map[string]string) string {
	type node struct {
		children map[string]*node
		size     int64
	}
	root := &node{children: map[string]*node{}}
	for name, body := range files {
		parts := strings.Split(name, "/")
		cur := root
		for _, part := range parts[:len(parts)-1] {
			child, ok := cur.children[part]
			if !ok {
				child = &node{children: map[string]*node{}}
				cur.children[part] = child
			}
			cur = child
		}
		cur.children[parts[len(parts)-1]] = &node{size: int64(len(body))}
	}
	var build func(n *node) []map[string]any
	build = func(n *node) []map[string]any {
		names := make([]string, 0, len(n.children))
		for name := range n.children {
			names = append(names, name)
		}
		sort.Strings(names)
		items := make([]map[string]any, 0, len(names))
		for _, name := range names {
			child := n.children[name]
			if len(child.children) == 0 {
				items = append(items, map[string]any{"name": name, "dir": false, "size": child.size})
				continue
			}
			items = append(items, map[string]any{"name": name, "dir": true, "size": 0, "items": build(child)})
		}
		return items
	}
	data, err := json.Marshal(map[string]any{
		"name": "package", "dir": true, "size": 0, "items": build(root),
	})
	if err != nil {
		panic(err)
	}
	return string(data)
}

func TestUnifiedPackageInstallUpdateUninstall(t *testing.T) {
	c, _ := testClient(t)
	c.commandPackages = t.TempDir()
	payload := map[string]string{
		"manifest.json": `{"schema":1,"commands":{"hello":{"run":["bin/hello"]}}}`,
		"bin/hello":     "source",
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Errorf("authorization = %q", r.Header.Get("Authorization"))
		}
		const prefix = "/fs/cloud/skills/private-package"
		switch {
		case r.URL.Path == prefix:
			w.Header().Set("X-UFS-Dir", "true")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(skillPackageTreeJSON(payload)))
		case strings.HasPrefix(r.URL.Path, prefix+"/"):
			body, ok := payload[strings.TrimPrefix(r.URL.Path, prefix+"/")]
			if !ok {
				http.NotFound(w, r)
				return
			}
			_, _ = w.Write([]byte(body))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	c.optsMu.Lock()
	c.opts.Host, c.authToken = server.URL, "test-token"
	c.optsMu.Unlock()
	if err := c.installCommandPackage(context.Background(), "private-package"); err != nil {
		t.Fatal(err)
	}
	def, ok := c.cmds.definition("hello")
	if !ok || def.ID != "skill:private-package:hello" {
		t.Fatalf("installed definition = %+v", def)
	}
	if body, err := os.ReadFile(filepath.Join(c.commandPackages, "private-package", "bin", "hello")); err != nil || string(body) != "source" {
		t.Fatalf("package file = %q, err = %v", body, err)
	}
	payload = map[string]string{
		"manifest.json": `{"schema":1,"commands":{"hello2":{"run":["bin/hello2"]}}}`,
		"bin/hello2":    "new-source",
	}
	if err := c.installCommandPackage(context.Background(), "private-package"); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.cmds.definition("hello"); ok {
		t.Fatal("old command survived update")
	}
	if _, ok := c.cmds.definition("hello2"); !ok {
		t.Fatal("updated command not registered")
	}
	if err := c.uninstallCommandPackage("private-package"); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.cmds.definition("hello2"); ok {
		t.Fatal("command survived uninstall")
	}
}

func TestUnifiedStreamOnlyBinaryEcho(t *testing.T) {
	c, _ := testClient(t)
	def := &hostCommand{
		ID: "config:raw-view", Alias: "raw-view", Source: commandSourceConfig, SkillID: "raw-package",
		Config: cfg.CommandConfig{
			Run: []string{os.Args[0], "-test.run=^TestCommandHelperProcess$"},
			Env: map[string]string{"AIC_COMMAND_HELPER": "stream"}, NoSandbox: true,
		},
		Streams: map[string]hostStream{"view": {Name: "view", AddressEnv: "AIC_TEST_STREAM_ADDR"}},
	}
	if err := allocateCommandStreams(def); err != nil {
		t.Fatal(err)
	}
	process := newStreamProcess(c, def)
	binding := commandStreamBinding{def: def, stream: def.Streams["view"], process: process}
	input := make(chan []byte, 1)
	input <- []byte{0, 1, 2, 0xff}
	received := make(chan []byte, 1)
	caller := testCaller()
	caller.Direct = true
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	errCh := make(chan error, 1)
	go func() {
		errCh <- c.streamHostCommand(ctx, caller, binding, input, func(raw []byte) error {
			received <- append([]byte(nil), raw...)
			return nil
		})
	}()
	var output []byte
	select {
	case output = <-received:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	close(input)
	if err := <-errCh; err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(output, []byte{0, 1, 2, 0xff}) {
		t.Fatalf("stream output = %x", output)
	}
}

func TestUnifiedPackageRejectsPathEscape(t *testing.T) {
	c, _ := testClient(t)
	root := t.TempDir()
	c.commandPackages = root
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Errorf("authorization = %q", r.Header.Get("Authorization"))
		}
		w.Header().Set("X-UFS-Dir", "true")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"name":"package","dir":true,"items":[{"name":"../escape","dir":false,"size":4}]}`))
	}))
	defer server.Close()
	c.optsMu.Lock()
	c.opts.Host, c.authToken = server.URL, "test-token"
	c.optsMu.Unlock()
	if err := c.installCommandPackage(context.Background(), "private-package"); err == nil {
		t.Fatal("package listing with escaping path was accepted")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(root), "escape")); err == nil {
		t.Fatal("package listing escaped the package root")
	}
}

func TestUnifiedManifestRejectsOwnerPolicy(t *testing.T) {
	manifest, err := parseCommandManifest([]byte(`{"schema":1,"commands":{"x":{"run":["x"],"no_sandbox":true}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := skillCommands("private", t.TempDir(), 1, manifest); err == nil {
		t.Fatal("skill manifest accepted no_sandbox")
	}
	empty := &hostCommand{Alias: "empty", Config: cfg.CommandConfig{Run: []string{""}}}
	if err := validateHostCommand(empty); err == nil {
		t.Fatal("empty program name was accepted")
	}
}

func TestUnifiedAliasPermissionGate(t *testing.T) {
	withGlobal(t, func(o *cfg.Options) { o.ExecPolicy = cfg.PolicyDeny; o.NetPolicy = cfg.PolicyOpen })
	service := mcp.NewServer(&mcp.Implementation{Name: "unified-gate", Version: "1"}, nil)
	mcp.AddTool(service, &mcp.Tool{Name: "ping"}, func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "pong"}}}, nil, nil
	})
	endpoint := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return service }, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true}))
	defer endpoint.Close()
	c, _ := testClient(t)
	c.perms.grantCmd("s1", "commands")
	run := func(script string) *protocol.Output {
		t.Helper()
		response := callTool(t, c, context.Background(), testCaller(), execRequest(script, 30000))
		result := decoded[protocol.Output](t, response.Result)
		return &result
	}
	if out := run("commands add gate-test --protocol mcp --url " + endpoint.URL); out.Attrs["exit_code"] != "0" {
		t.Fatalf("management grant did not allow add: %+v", out.Attrs)
	}
	for _, script := range []string{"gate-test ping", "commands start gate-test", "commands mcp gate-test tools"} {
		if out := run(script); out.Attrs["exit_code"] != "126" {
			t.Fatalf("%s bypassed alias gate: %+v", script, out.Attrs)
		}
	}
	c.perms.grantCmd("s1", "gate-test")
	for _, script := range []string{"gate-test ping", "commands mcp gate-test tools"} {
		if out := run(script); out.Attrs["exit_code"] != "0" {
			t.Fatalf("authorized %s failed: %+v", script, out.Attrs)
		}
	}
}

func TestPreserveCommandPackageArtifacts(t *testing.T) {
	old := t.TempDir()
	if err := os.MkdirAll(filepath.Join(old, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(old, "bin", "browser"), []byte("built"), 0o755); err != nil {
		t.Fatal(err)
	}
	fresh := t.TempDir()
	preserveCommandPackageArtifacts(old, fresh)
	data, err := os.ReadFile(filepath.Join(fresh, "bin", "browser"))
	if err != nil || string(data) != "built" {
		t.Fatalf("build artifact was not preserved: %v %q", err, data)
	}
	shipped := t.TempDir()
	if err := os.MkdirAll(filepath.Join(shipped, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(shipped, "bin", "browser"), []byte("shipped"), 0o755); err != nil {
		t.Fatal(err)
	}
	preserveCommandPackageArtifacts(old, shipped)
	data, err = os.ReadFile(filepath.Join(shipped, "bin", "browser"))
	if err != nil || string(data) != "shipped" {
		t.Fatalf("shipped artifact was overwritten: %v %q", err, data)
	}
}
