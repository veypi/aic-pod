package vcore

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ---- curl（§5.4）----

// curl [-L] [-X <method>] [-d <data>] [-H "Name: Value"]... [-A <ua>] [-o <path>] <url>
// [--max-size <MB>] [--max-time <sec>]：仅 http/https。方法白名单
// GET/POST/PUT/PATCH/DELETE/HEAD（默认 GET；仅未显式 -X 时 -d 隐式 POST）；-d 为请求体
// （GET/HEAD 携带报错）；-H 自定义请求头（可重复，后者覆盖）；-A 设置 User-Agent
// （覆盖 -H 的同名头）；--max-time 请求超时秒数（1~600，0 或省略=平台默认上限）。
//
//   - 带 -o：流式写入 <path>（目标必须不存在；父目录自动创建；超限中止并删除
//     半成品文件）。写文件语义——调用方需先经 fs 门控（file service）。
//   - 不带 -o：输出进 content，经 env.Tasks 任务托管（输出落盘日志、请求超时
//     自动后台化、前 1000 行截断返回，bg_wait 续取）。二进制内容嗅探即拒
//     （中止下载，不落盘），提示改用 -o——对齐 curl CLI 拒绝向终端倾倒二进制的语义。
//
// -L 跟随重定向（可选；Fetcher 默认已跟随，每跳均过 SSRF 校验）。
// SSRF 防护由 Fetcher 实现方注入（cloud 严格 / 物理 host 不限制）。
// --max-size 默认 1024MB，上限 10240MB。
//
// flag 宽容：未声明的 flag（如真实 curl 的 -sL/-k/-i 等）静默剥离当作不存在，
// 不报受限错误（dropUnknownFlags）——curl 是高频工具，AI 常带真实 curl 习惯参数；
// 其余虚拟指令（git/json/commands 等）仍保持严格受限反馈。

// curlBoolFlags / curlValueFlags / curlListFlags 是 curl 声明的 flag 子集
// （与 cmdCurl 的 argvSpec 一致），dropUnknownFlags 据此剥离未声明 flag。
// curlKnownValueFlags 是未实现但已知带值的 flag：剥离时连带其后 token 一起剥
// （否则值会残留成位置参数——如 -w "format" url → "format" 变位置参数报
// unexpected argument）。
var (
	curlBoolFlags       = map[string]bool{"-L": true}
	curlValueFlags      = map[string]bool{"-o": true, "--max-size": true, "-X": true, "-d": true, "-A": true, "--max-time": true}
	curlListFlags       = map[string]bool{"-H": true}
	curlKnownValueFlags = map[string]bool{
		"-w": true, "--write-out": true, "-b": true, "--cookie": true, "-u": true, "--user": true,
		"-e": true, "--referer": true, "--retry": true, "--connect-timeout": true, "-T": true,
		"--upload-file": true, "--data-urlencode": true, "--limit-rate": true, "--speed-limit": true,
	}
)

// dropUnknownFlags 剥离 curl 未声明的 flag（用户语义：不支持的参数当不存在）：
//   - 已知 flag（-L/-o/--max-size/-X/-d/-A/-H/--max-time）与纯已知 bool 组合（如 -LL）保留；
//   - 未实现但已知带值的 flag（-w/--retry 等）：flag 与其后值一起剥离；
//   - 其余未知 flag：只剥 flag 自身（无法静态判断是否带值，多出的位置参数仍由
//     parseArgv 的 minPos/maxPos 兜底）。
func dropUnknownFlags(argv []string) []string {
	out := make([]string, 0, len(argv))
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		if strings.HasPrefix(a, "-") && a != "-" {
			if curlBoolFlags[a] || curlValueFlags[a] || curlListFlags[a] {
				out = append(out, a)
				continue
			}
			if curlKnownValueFlags[a] {
				if i+1 < len(argv) && !strings.HasPrefix(argv[i+1], "-") {
					i++ // 连值一起剥
				}
				continue
			}
			// 单横线组合：全部为已知 bool 时保留（对齐 parseArgv 的展开语义）
			if !strings.HasPrefix(a, "--") && len(a) > 2 {
				allKnown := true
				for _, c := range a[1:] {
					if !curlBoolFlags["-"+string(c)] {
						allKnown = false
						break
					}
				}
				if allKnown {
					out = append(out, a)
					continue
				}
			}
			continue // 未知 flag：当作不存在
		}
		out = append(out, a)
	}
	return out
}

// curlSniffBytes 是无 -o 形态的二进制嗅探窗（前 8KB 含 null 字节即判二进制）。
const curlSniffBytes = 8192

// binaryOutputError 标记「无 -o 且内容为二进制」的拒绝（任务体内中止下载用）。
type binaryOutputError struct{ mime string }

func (e *binaryOutputError) Error() string {
	return fmt.Sprintf("binary output (%s) refused: not written to content; use -o <path> to save the file", e.mime)
}

func cmdCurl(ctx context.Context, env *Env, argv []string) (*Result, error) {
	pa, err := parseArgv("curl", argvSpec{
		bools:  map[string]bool{"-L": true}, // 跟随重定向（Fetcher 默认已跟随）
		values: map[string]bool{"-o": true, "--max-size": true, "-X": true, "-d": true, "-A": true, "--max-time": true},
		lists:  map[string]bool{"-H": true}, // 自定义请求头，可重复
		minPos: 1, maxPos: 1,
	}, dropUnknownFlags(argv))
	if err != nil {
		return nil, err
	}
	dst := pa.values["-o"]
	rawurl := pa.pos[0]

	// scheme 判定（RFC 3986 大小写不敏感归一）：
	// http(s):// 为标准形；{host}:/{path} 单斜杠预留形按 scheme 拒绝。
	var scheme string
	if idx := strings.Index(rawurl, "://"); idx >= 0 {
		scheme = strings.ToLower(rawurl[:idx])
	} else if ci := strings.Index(rawurl, ":"); ci > 0 &&
		(strings.Index(rawurl, "/") < 0 || ci < strings.Index(rawurl, "/")) {
		scheme = strings.ToLower(rawurl[:ci])
	} else {
		return nil, execErr("curl", "invalid url %q: missing scheme", rawurl)
	}
	switch scheme {
	case "http", "https":
	default:
		// cloud: 与 {host_id}: 源 scheme 预留（§1.3，仅设计不实现）
		return nil, execErr("curl", "scheme not yet supported: %s", scheme)
	}

	maxSizeMB := 1024
	if v := pa.values["--max-size"]; v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return nil, execErr("curl", "--max-size must be >= 1, got %s", v)
		}
		if n > 10240 {
			n = 10240
		}
		maxSizeMB = n
	}

	req, err := buildHTTPReq(pa)
	if err != nil {
		return nil, err
	}

	// --max-time 请求超时（秒）：1~600，非法报错；0/省略 = 平台默认上限（执行层 600s）
	maxTime := time.Duration(0)
	if v := pa.values["--max-time"]; v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return nil, execErr("curl", "--max-time must be >= 1 seconds, got %s", v)
		}
		if n > 600 {
			return nil, execErr("curl", "--max-time exceeds platform limit (600s), got %d", n)
		}
		maxTime = time.Duration(n) * time.Second
	}

	if dst == "/dev/null" {
		// 用户意图：丢弃响应体（对齐真实 curl -o /dev/null），不落盘不建文件
		return curlDiscard(ctx, env, req, maxSizeMB, maxTime)
	}
	if dst == "" {
		return curlToContent(ctx, env, req, maxSizeMB, maxTime)
	}
	return curlToFile(ctx, env, req, dst, maxSizeMB, maxTime)
}

// curlDiscard 实现 -o /dev/null 形态：请求后丢弃响应体（仍受大小限制），
// Content 返回字节数摘要。
func curlDiscard(ctx context.Context, env *Env, req HTTPReq, maxSizeMB int, maxTime time.Duration) (*Result, error) {
	if env.Fetcher == nil {
		return nil, execErr("curl", "curl is not available on this host")
	}
	if maxTime > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, maxTime)
		defer cancel()
	}
	body, totalSize, err := env.Fetcher.Fetch(ctx, req)
	if err != nil {
		return nil, execErr("curl", "fetch %s: %s", req.URL, err)
	}
	defer body.Close()
	maxBytes := int64(maxSizeMB) << 20
	n, err := io.Copy(io.Discard, io.LimitReader(body, maxBytes+1))
	if n > maxBytes {
		actualMB := n >> 20
		if totalSize >= 0 {
			actualMB = totalSize >> 20
		}
		return nil, execErr("curl", "size limit exceeded (%dMB > %dMB)", actualMB, maxSizeMB)
	}
	if err != nil {
		return nil, execErr("curl", "read %s: %s", req.URL, err)
	}
	r := newResult("curl", "")
	r.Content = fmt.Sprintf("discarded response from %s (%d bytes)", req.URL, n)
	r.set("bytes", n)
	return r, nil
}

// curlMethods 是 curl 支持的方法白名单（大写）。
var curlMethods = map[string]bool{
	http.MethodGet: true, http.MethodPost: true, http.MethodPut: true,
	http.MethodPatch: true, http.MethodDelete: true, http.MethodHead: true,
}

// buildHTTPReq 从解析后的 argv 构造请求：
//   - 方法：-X <METHOD>（默认 GET；仅未显式 -X 时 -d 存在才隐式 POST，对齐 curl 语义）；
//   - 体：-d <data>（GET/HEAD 等无体方法携带 -d 报错）；
//   - 头：-H "Name: Value"（可重复，后者覆盖同名）。
func buildHTTPReq(pa *parsedArgv) (HTTPReq, error) {
	method := strings.ToUpper(strings.TrimSpace(pa.values["-X"]))
	explicit := method != ""
	if !explicit {
		method = http.MethodGet
	}
	body := []byte(pa.values["-d"])
	if len(body) > 0 && !explicit {
		method = http.MethodPost // -d 隐式 POST（仅未显式 -X 时）
	}
	if !curlMethods[method] {
		return HTTPReq{}, execErr("curl", "method %q not supported (GET/POST/PUT/PATCH/DELETE/HEAD)", method)
	}
	if len(body) > 0 && (method == http.MethodGet || method == http.MethodHead) {
		return HTTPReq{}, execErr("curl", "request body (-d) is not allowed with method %s", method)
	}
	var headers map[string]string
	for _, h := range pa.lists["-H"] {
		k, v, ok := strings.Cut(h, ":")
		if !ok {
			return HTTPReq{}, execErr("curl", "invalid header %q (want \"Name: Value\")", h)
		}
		if headers == nil {
			headers = map[string]string{}
		}
		headers[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	// -A 设置 User-Agent（覆盖 -H 的同名头，对齐 curl 语义）
	if ua := strings.TrimSpace(pa.values["-A"]); ua != "" {
		if headers == nil {
			headers = map[string]string{}
		}
		headers["User-Agent"] = ua
	}
	return HTTPReq{Method: method, URL: pa.pos[0], Headers: headers, Body: body}, nil
}

// curlToFile 实现 -o 形态：流式写入文件（写文件语义，fs 门控由调用方前置）。
func curlToFile(ctx context.Context, env *Env, req HTTPReq, dst string, maxSizeMB int, maxTime time.Duration) (*Result, error) {
	if env.VFS == nil {
		return nil, execErr("curl", "file service is not enabled (curl -o requires the fs tool)")
	}
	abs, err := env.Resolve(dst)
	if err != nil {
		return nil, execErr("curl", "%s", err)
	}
	if err := env.CheckPath("curl", abs); err != nil {
		return nil, err
	}
	if err := env.CheckPolicy("curl -o", abs, true); err != nil {
		return nil, err
	}
	if _, err := env.VFS.Stat(abs); err == nil {
		return nil, execErr("curl", "destination %s already exists", abs)
	}
	if env.Fetcher == nil {
		return nil, execErr("curl", "curl is not available on this host")
	}
	if maxTime > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, maxTime)
		defer cancel()
	}

	body, totalSize, err := env.Fetcher.Fetch(ctx, req)
	if err != nil {
		return nil, execErr("curl", "fetch %s: %s", req.URL, err)
	}
	defer body.Close()

	if err := env.VFS.MkdirAll(dirOf(abs), 0o755); err != nil {
		return nil, execErr("curl", "%s", err)
	}
	f, err := env.VFS.Create(abs)
	if err != nil {
		return nil, execErr("curl", "cannot create %s: %s", abs, err)
	}
	maxBytes := int64(maxSizeMB) << 20
	n, copyErr := io.Copy(f, io.LimitReader(body, maxBytes+1))
	if closeErr := f.Close(); copyErr == nil {
		copyErr = closeErr
	}
	if n > maxBytes {
		_ = env.VFS.RemoveAll(abs)
		actualMB := n >> 20
		if totalSize >= 0 {
			actualMB = totalSize >> 20
		}
		return nil, execErr("curl", "size limit exceeded (%dMB > %dMB)", actualMB, maxSizeMB)
	}
	if copyErr != nil {
		_ = env.VFS.RemoveAll(abs)
		return nil, execErr("curl", "write %s: %s", abs, copyErr)
	}
	r := newResult("curl", abs)
	r.Content = fmt.Sprintf("downloaded %s to %s (%d bytes)", req.URL, abs, n)
	r.set("bytes", n)
	return r, nil
}

// curlToContent 实现无 -o 形态：输出进 content，经 env.Tasks 任务托管。
// 任务体先嗅探前 8KB：二进制 → 中止下载返回 binaryOutputError（同步路径
// 原样上报；后台路径错误写入日志，bg_wait 可见）；文本 → 落盘日志。
func curlToContent(ctx context.Context, env *Env, req HTTPReq, maxSizeMB int, maxTime time.Duration) (*Result, error) {
	if env.Fetcher == nil {
		return nil, execErr("curl", "curl is not available on this host")
	}
	if env.Tasks == nil {
		return nil, execErr("curl", "output to content is not supported on this environment (use -o <path>)")
	}
	taskID := env.TaskID
	if taskID == "" {
		return nil, execErr("curl", "missing task id (environment misconfigured)")
	}
	maxBytes := int64(maxSizeMB) << 20
	res, err := env.Tasks.StartTask(ctx, TaskOptions{
		ID:      taskID,
		Command: "curl " + req.URL,
		Run: func(tctx context.Context, out io.Writer) error {
			if maxTime > 0 {
				var cancel context.CancelFunc
				tctx, cancel = context.WithTimeout(tctx, maxTime)
				defer cancel()
			}
			body, totalSize, err := env.Fetcher.Fetch(tctx, req)
			if err != nil {
				return fmt.Errorf("fetch %s: %s", req.URL, err)
			}
			defer body.Close()
			// 嗅探窗：二进制即拒（中止下载，不写日志）
			head := make([]byte, curlSniffBytes)
			n, _ := io.ReadFull(io.LimitReader(body, curlSniffBytes), head)
			head = head[:n]
			if !isTextContent(head) {
				return &binaryOutputError{mime: detectMIME(head, req.URL)}
			}
			if _, err := out.Write(head); err != nil {
				return err
			}
			m, err := io.Copy(out, io.LimitReader(body, maxBytes+1-int64(n)))
			total := int64(n) + m
			if total > maxBytes {
				actualMB := total >> 20
				if totalSize >= 0 {
					actualMB = totalSize >> 20
				}
				return fmt.Errorf("size limit exceeded (%dMB > %dMB)", actualMB, maxSizeMB)
			}
			return err
		},
	})
	if err != nil {
		return nil, execErr("curl", "%s", err)
	}
	r := newResult("curl", "")
	r.Content = res.Content
	r.set("rows", res.Lines)
	r.set("truncated", res.Truncated)
	if res.LogPath != "" {
		r.Attrs["path"] = res.LogPath
	}
	if res.Background {
		r.set("background", true)
		r.Attrs["id"] = res.ID
	}
	return r, nil
}
