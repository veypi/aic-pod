package host

// cua run 脚本执行（§5.10 扩展）：整个 JS 脚本随一次 exec 调用下发，host 本地
// 逐步执行（步骤间零服务端往返），结束一次性返回 transcript。
//
// 链路：exec cua run → runCuaScript（本文件）→ 本地 TCP 桥（127.0.0.1 随机端口 +
// 随机 token，仅脚本生命周期内存活）⇄ node 子进程（cua_runner.mjs，go:embed 内嵌）
// → cuaRt.call（复用持久 MCP 连接：callMu 串行 / 浏览器绑定 / token 缓存）。
//
//   - node 二进位：CUA_NODE_PATH → AIC_NODE_BIN（Electron 主进程注入
//     process.execPath，配 ELECTRON_RUN_AS_NODE=1 当纯 node 用，三平台 Electron
//     包自带，零新增依赖）→ PATH node；都没有则报错暴露（不静默降级）；
//   - 脚本入口：--code 内联 / --file <host 绝对路径>（stdin 传入，免 argv 长度
//     与转义问题）；以 AsyncFunction('cua','console',code) 执行，支持 await/return；
//   - snapshot 特化（__snapshot）：复用落盘契约（.cua/snap-*.txt/.png），返回脚本
//     友好对象（elements 精简字段全量留给脚本本地过滤，不占模型 token）；
//   - 护栏：脚本总时长随 exec ctx（≤300s，ctx 取消即杀 node）；runner 侧步数
//     上限 500 防死循环；桥单行 ≤32MB；
//   - 产物：transcript 全文 JSONL 落盘 .cua/run-<ts>.jsonl；返回精简（每步一行），
//     脚本未捕获异常 → StateError 并带回已执行步骤。
//
// 安全模型：run 恒 Danger(3) 逐次审批（脚本可含任意动作无法静态分级，审批界面
// 展示脚本全文）。node 子进程不受 exec_procs 沙箱约束（与 cua 本体同边界：本质
// 是控制本机 GUI 的宿主体外能力，且脚本可自由 require node 标准库）。

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/veypi/aic-pod/libs/proto"
)

//go:embed cua_runner.mjs
var cuaRunnerJS []byte

// cuaRunMaxCode 脚本全文上限（--file 读取/--code 内联同限）。
const cuaRunMaxCode = 512 * 1024

// cuaRunMaxLine 桥单行上限（AX 树/elements 全量可能很大）。
const cuaRunMaxLine = 32 * 1024 * 1024

// cuaRunMaxOut node stdout 收集上限（transcript 标记行在末尾，超帽丢弃头部）。
const cuaRunMaxOut = 4 * 1024 * 1024

// ---- 桥协议 ----

type cuaRunReq struct {
	ID    int            `json:"id"`
	Token string         `json:"token"`
	Tool  string         `json:"tool"`
	Args  map[string]any `json:"args"`
}

type cuaRunResp struct {
	ID         int            `json:"id"`
	OK         bool           `json:"ok"`
	Text       string         `json:"text,omitempty"`
	Structured map[string]any `json:"structured,omitempty"`
	Error      string         `json:"error,omitempty"`
}

// cuaRunStep 是 runner 上报的单步 transcript 条目。
type cuaRunStep struct {
	I     int    `json:"i"`
	Op    string `json:"op"`
	Brief string `json:"brief"`
	Ms    int64  `json:"ms"`
	OK    bool   `json:"ok"`
	Err   string `json:"err,omitempty"`
}

type cuaRunResult struct {
	Steps  []cuaRunStep    `json:"steps"`
	Result json.RawMessage `json:"result"`
	Error  string          `json:"error"`
}

const cuaRunResultMark = "\n__CUA_RESULT__"

// findNodeBin 探测 node 运行时：CUA_NODE_PATH（纯 node）→ AIC_NODE_BIN
// （Electron 二进位，ELECTRON_RUN_AS_NODE=1 当 node 用）→ PATH node。
func findNodeBin() (bin string, extraEnv []string, err error) {
	if p := os.Getenv("CUA_NODE_PATH"); p != "" {
		if _, serr := os.Stat(p); serr == nil {
			return p, nil, nil
		}
	}
	if p := os.Getenv("AIC_NODE_BIN"); p != "" {
		if _, serr := os.Stat(p); serr == nil {
			return p, []string{"ELECTRON_RUN_AS_NODE=1"}, nil
		}
	}
	if p, lerr := exec.LookPath("node"); lerr == nil {
		return p, nil, nil
	}
	return "", nil, fmt.Errorf("node runtime not found (set CUA_NODE_PATH, or run under Electron shell which provides AIC_NODE_BIN)")
}

// tailWriter 只保留末尾 cap 字节（transcript 标记行在末尾，头部 stdout 噪音可丢）。
type tailWriter struct {
	buf []byte
	cap int
}

func (w *tailWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	if len(w.buf) > w.cap {
		w.buf = append([]byte{}, w.buf[len(w.buf)-w.cap:]...)
	}
	return len(p), nil
}

// runCuaScript 执行 cua run（runCua 特化分支）。
func (c *Client) runCuaScript(ctx context.Context, sid string, req *proto.ToolRequest, mapped *cuaCall) *proto.ToolResponse {
	fail := func(err error) *proto.ToolResponse {
		return &proto.ToolResponse{MsgID: req.MsgID, State: proto.StateError, Error: "cua run: " + err.Error()}
	}
	// 1. 脚本来源
	code := mapped.code
	if mapped.file != "" {
		if !filepath.IsAbs(mapped.file) {
			return fail(fmt.Errorf("--file 需 host 绝对路径: %s", mapped.file))
		}
		data, err := os.ReadFile(mapped.file)
		if err != nil {
			return fail(fmt.Errorf("read --file: %w", err))
		}
		if len(data) > cuaRunMaxCode {
			return fail(fmt.Errorf("script too large: %d bytes (cap %d)", len(data), cuaRunMaxCode))
		}
		code = string(data)
	}
	if strings.TrimSpace(code) == "" {
		return fail(fmt.Errorf("empty script"))
	}
	// 2. node 运行时
	nodeBin, nodeEnv, err := findNodeBin()
	if err != nil {
		return fail(err)
	}
	// 3. 产物目录 + runner 落盘（内容静态，每次覆写保持与二进位同版）
	cuaDir := filepath.Join(sessionWorkDir(sid), ".cua")
	if err := os.MkdirAll(cuaDir, 0o700); err != nil {
		return fail(err)
	}
	runnerPath := filepath.Join(cuaDir, "cua-runner.mjs")
	if err := os.WriteFile(runnerPath, cuaRunnerJS, 0o600); err != nil {
		return fail(fmt.Errorf("write runner: %w", err))
	}
	// 4. 本地桥（随机端口 + 随机 token）
	tokenBytes := make([]byte, 16)
	if _, err := rand.Read(tokenBytes); err != nil {
		return fail(err)
	}
	token := hex.EncodeToString(tokenBytes)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fail(fmt.Errorf("bridge listen: %w", err))
	}
	defer ln.Close()
	bridge := &cuaRunBridge{sid: sid, token: token, cuaDir: cuaDir, logf: c.logf}
	go bridge.serve(ctx, ln)

	// 5. spawn node（ctx 取消即杀；stdin 传脚本全文）
	cmd := exec.CommandContext(ctx, nodeBin, runnerPath, ln.Addr().String(), token)
	cmd.Stdin = strings.NewReader(code)
	cmd.Env = append(os.Environ(), nodeEnv...)
	out := &tailWriter{cap: cuaRunMaxOut}
	cmd.Stdout = out
	cmd.Stderr = &lineLogWriter{logf: c.logf, tag: "[cua run]"}
	if err := cmd.Start(); err != nil {
		return fail(fmt.Errorf("start node: %w", err))
	}
	runErr := cmd.Wait()
	ln.Close() // 不再接受新连接；在途 handleConn 随脚本结束自然收尾

	// 6. 解析结果标记行
	stdout := string(out.buf)
	idx := strings.LastIndex(stdout, cuaRunResultMark)
	if idx < 0 {
		// 无标记行 = runner 自身崩溃（语法错误之外级别的故障）
		tail := strings.TrimSpace(stdout)
		if len(tail) > 2000 {
			tail = tail[len(tail)-2000:]
		}
		if runErr != nil {
			return fail(fmt.Errorf("runner exited: %v; output tail: %s", runErr, tail))
		}
		return fail(fmt.Errorf("runner produced no result marker; output tail: %s", tail))
	}
	var rr cuaRunResult
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout[idx+len(cuaRunResultMark):])), &rr); err != nil {
		return fail(fmt.Errorf("parse result: %w", err))
	}

	// 7. transcript 全文落盘 + 精简返回
	logPath := filepath.Join(cuaDir, fmt.Sprintf("run-%d.jsonl", time.Now().UnixMilli()))
	{
		var jl strings.Builder
		for _, s := range rr.Steps {
			if data, err := json.Marshal(s); err == nil {
				jl.Write(data)
				jl.WriteByte('\n')
			}
		}
		_ = os.WriteFile(logPath, []byte(jl.String()), 0o600) // 落盘失败不遮主结果
	}
	content := cuaRunTranscript(&rr, logPath)
	resp := &proto.ToolResponse{MsgID: req.MsgID, State: proto.StateCompleted, Content: content,
		Attrs: map[string]string{"path": logPath}}
	if rr.Error != "" {
		resp.State = proto.StateError
		resp.Error = "cua run: script failed: " + rr.Error
	}
	return resp
}

// cuaRunTranscript 生成精简 transcript（每步一行，总帽 ~40KB）。
func cuaRunTranscript(rr *cuaRunResult, logPath string) string {
	var b strings.Builder
	status := "OK"
	if rr.Error != "" {
		status = "FAILED"
	}
	fmt.Fprintf(&b, "cua run: %s (%d steps)\n", status, len(rr.Steps))
	for _, s := range rr.Steps {
		mark := "ok"
		if !s.OK {
			mark = "ERR"
		}
		line := fmt.Sprintf("[%d] %s %s → %s (%dms)\n", s.I, s.Op, s.Brief, mark, s.Ms)
		if s.Err != "" {
			line = fmt.Sprintf("[%d] %s %s → ERR (%dms): %s\n", s.I, s.Op, s.Brief, s.Ms, truncStr(s.Err, 300))
		}
		if b.Len()+len(line) > 40*1024 {
			fmt.Fprintf(&b, "…（省略 %d 步，全文见 log）\n", len(rr.Steps)-s.I)
			break
		}
		b.WriteString(line)
	}
	if rr.Error != "" {
		fmt.Fprintf(&b, "[error] %s\n", truncStr(rr.Error, 500))
	}
	if len(rr.Result) > 0 && string(rr.Result) != "null" {
		fmt.Fprintf(&b, "[result] %s\n", truncStr(string(rr.Result), 2000))
	}
	b.WriteString("[log] " + logPath + "\n")
	return b.String()
}

// ---- 桥服务 ----

type cuaRunBridge struct {
	sid    string
	token  string
	cuaDir string
	logf   func(string, ...any)
}

func (b *cuaRunBridge) serve(ctx context.Context, ln net.Listener) {
	go func() { <-ctx.Done(); ln.Close() }()
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go b.handleConn(ctx, conn)
	}
}

func (b *cuaRunBridge) handleConn(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	reader := bufio.NewReaderSize(conn, 4*1024*1024)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if len(line) > cuaRunMaxLine {
			b.logf("[cua run] bridge line overflow (%d bytes)", len(line))
			return
		}
		var r cuaRunReq
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			continue
		}
		resp := b.dispatch(ctx, &r)
		data, _ := json.Marshal(resp)
		conn.SetWriteDeadline(time.Now().Add(120 * time.Second))
		if _, err := conn.Write(append(data, '\n')); err != nil {
			return
		}
	}
}

// dispatch 路由桥请求：__ 前缀内建工具本地处理，其余透传 cuaRt.call
// （浏览器家族沿用 runCua 的 session/绑定注入语义）。
func (b *cuaRunBridge) dispatch(ctx context.Context, r *cuaRunReq) *cuaRunResp {
	resp := &cuaRunResp{ID: r.ID}
	if r.Token != b.token {
		resp.Error = "bad token"
		return resp
	}
	if r.Args == nil {
		r.Args = map[string]any{}
	}
	var text string
	var structured map[string]any
	var err error
	switch r.Tool {
	case "__hello":
		structured = map[string]any{"platform": runtime.GOOS, "session": "aic-" + b.sid}
	case "__ime":
		// 键盘类动作前的 IME 护栏（cua_ime.go）：脚本侧 cua.key/hotkey/type
		// 调用前显式触发；已是英文时零开销（一次 defaults 读取）。
		structured = map[string]any{"note": imeGuard()}
	case "__snapshot":
		structured, err = b.snapshot(ctx, r.Args)
	default:
		text, structured, err = b.passthrough(ctx, r.Tool, r.Args)
	}
	if err != nil {
		resp.Error = err.Error()
		return resp
	}
	resp.OK = true
	resp.Text = text
	resp.Structured = structured
	return resp
}

// passthrough 透传 MCP 工具（浏览器家族注入 session/绑定，应答提取绑定）。
func (b *cuaRunBridge) passthrough(ctx context.Context, tool string, args map[string]any) (string, map[string]any, error) {
	switch tool {
	case "browser_prepare", "get_browser_state", "browser_navigate", "browser_click",
		"browser_type", "browser_dialog", "browser_pointer", "browser_set_input_files",
		"browser_download", "end_session":
		if _, has := args["session"]; !has {
			args["session"] = "aic-" + b.sid
		}
	}
	switch tool {
	case "browser_navigate", "browser_click", "browser_type", "browser_dialog",
		"browser_pointer", "browser_set_input_files", "browser_download":
		tid, tab := cuaRt.browserBinding()
		if tid == "" || tab == "" {
			return "", nil, fmt.Errorf("无浏览器绑定：先 bprepare/browserState 绑定")
		}
		args["target_id"] = tid
		args["tab_id"] = tab
	case "get_browser_state":
		if _, has := args["pid"]; !has {
			if pid := cuaRt.browserPidValue(); pid != 0 {
				args["pid"] = pid
			}
		}
		if _, has := args["window_id"]; !has {
			if pid, ok := args["pid"].(float64); ok && pid != 0 {
				if wres, werr := cuaRt.call(ctx, "list_windows", map[string]any{"pid": int(pid)}); werr == nil {
					if wid := cuaFirstWindowID(wres.StructuredContent); wid != 0 {
						args["window_id"] = wid
					}
				}
			}
		}
	}
	res, err := cuaRt.call(ctx, tool, args)
	if err != nil {
		return "", nil, err
	}
	// bprepare/browser-state：提取绑定（与 runCua 同语义）
	if tool == "browser_prepare" || tool == "get_browser_state" {
		if res.StructuredContent != nil {
			if f, ok := res.StructuredContent["prepared_pid"].(float64); ok && f > 0 {
				cuaRt.setBrowserPid(int(f))
			}
			found := map[string]string{}
			cuaFindStrKeys(res.StructuredContent,
				map[string]bool{"target_id": true, "tab_id": true, "active_tab_id": true}, found, 0)
			tab := found["tab_id"]
			if tab == "" {
				tab = found["active_tab_id"]
			}
			if found["target_id"] != "" {
				cuaRt.setBrowserBinding(found["target_id"], tab)
			}
		}
	}
	var texts []string
	for _, item := range res.Content {
		if item.Type == "text" && item.Text != "" {
			texts = append(texts, item.Text)
		}
	}
	return strings.Join(texts, "\n"), res.StructuredContent, nil
}

// snapshot 脚本特化：同落盘契约（txt/png），返回脚本对象
// {title,pid,window_id,bounds,elements(精简),text_path,png_path?,degraded?}。
func (b *cuaRunBridge) snapshot(ctx context.Context, args map[string]any) (map[string]any, error) {
	pid, _ := args["pid"].(float64)
	wid, _ := args["window_id"].(float64)
	png, _ := args["png"].(bool)
	callArgs := map[string]any{"pid": int(pid), "window_id": int(wid)}
	base := filepath.Join(b.cuaDir, fmt.Sprintf("snap-%d", time.Now().UnixMilli()))
	shotPath := base + ".png"
	txtPath := base + ".txt"
	if png {
		callArgs["include_screenshot"] = true
		callArgs["screenshot_out_file"] = shotPath
	}
	res, err := cuaRt.call(ctx, "get_window_state", callArgs)
	if err != nil {
		return nil, err
	}
	sc := res.StructuredContent
	tree := ""
	if sc != nil {
		tree, _ = sc["tree_markdown"].(string)
	}
	if tree == "" {
		for _, item := range res.Content {
			if item.Type == "text" {
				tree = item.Text
				break
			}
		}
	}
	elements := cuaSnapshotElements(sc)
	if tree != "" || len(elements) > 0 {
		if err := os.WriteFile(txtPath, []byte(cuaSnapshotDoc(sc, tree, elements)), 0o600); err != nil {
			return nil, fmt.Errorf("write snapshot txt: %w", err)
		}
	}
	out := map[string]any{
		"pid":       int(pid),
		"window_id": int(wid),
		"elements":  cuaRunTrimElements(elements),
		"text_path": txtPath,
	}
	if len(elements) > 0 {
		out["title"], _ = elements[0]["label"].(string)
	}
	if sc != nil {
		if wb, ok := sc["window_bounds"].(map[string]any); ok {
			out["bounds"] = wb
		}
		if degraded, _ := sc["degraded"].(bool); degraded {
			out["degraded"] = scVal(sc, "degraded_reason")
		}
	}
	if png {
		if _, err := os.Stat(shotPath); err != nil {
			return nil, fmt.Errorf("snapshot png: driver did not produce screenshot: %w", err)
		}
		out["png_path"] = shotPath
	}
	return out, nil
}

// cuaRunTrimElements 精简 elements 字段（脚本本地过滤用，去长尾键）。
func cuaRunTrimElements(elements []map[string]any) []map[string]any {
	out := make([]map[string]any, 0, len(elements))
	for _, e := range elements {
		t := map[string]any{}
		for _, k := range []string{"element_index", "element_token", "role", "label", "value", "description", "frame", "depth"} {
			if v, ok := e[k]; ok {
				t[k] = v
			}
		}
		out = append(out, t)
	}
	return out
}

// lineLogWriter 按行转写 stderr 到宿主日志。
type lineLogWriter struct {
	logf func(string, ...any)
	tag  string
	buf  bytes.Buffer
}

func (w *lineLogWriter) Write(p []byte) (int, error) {
	w.buf.Write(p)
	for {
		i := bytes.IndexByte(w.buf.Bytes(), '\n')
		if i < 0 {
			break
		}
		line := string(w.buf.Bytes()[:i])
		w.buf.Next(i + 1)
		w.logf("%s %s", w.tag, line)
	}
	return len(p), nil
}

// 确保 io 引用保留（tailWriter/lineLogWriter 实现 io.Writer）。
var _ io.Writer = (*tailWriter)(nil)
var _ io.Writer = (*lineLogWriter)(nil)
