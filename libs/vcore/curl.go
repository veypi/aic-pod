package vcore

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// ---- curl（§5.4）----

// curl [-L] [-o <path>] <url> [--max-size <MB>]：仅 http/https，GET。
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
		values: map[string]bool{"-o": true, "--max-size": true},
		minPos: 1, maxPos: 1,
	}, argv)
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

	if dst == "" {
		return curlToContent(ctx, env, rawurl, maxSizeMB)
	}
	return curlToFile(ctx, env, rawurl, dst, maxSizeMB)
}

// curlToFile 实现 -o 形态：流式写入文件（写文件语义，fs 门控由调用方前置）。
func curlToFile(ctx context.Context, env *Env, rawurl, dst string, maxSizeMB int) (*Result, error) {
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
	if _, err := env.VFS.Stat(abs); err == nil {
		return nil, execErr("curl", "destination %s already exists", abs)
	}
	if env.Fetcher == nil {
		return nil, execErr("curl", "curl is not available on this host")
	}

	body, totalSize, err := env.Fetcher.Get(ctx, rawurl)
	if err != nil {
		return nil, execErr("curl", "fetch %s: %s", rawurl, err)
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
	r.Content = fmt.Sprintf("downloaded %s to %s (%d bytes)", rawurl, abs, n)
	r.set("bytes", n)
	return r, nil
}

// curlToContent 实现无 -o 形态：输出进 content，经 env.Tasks 任务托管。
// 任务体先嗅探前 8KB：二进制 → 中止下载返回 binaryOutputError（同步路径
// 原样上报；后台路径错误写入日志，bg_wait 可见）；文本 → 落盘日志。
func curlToContent(ctx context.Context, env *Env, rawurl string, maxSizeMB int) (*Result, error) {
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
		Command: "curl " + rawurl,
		Run: func(tctx context.Context, out io.Writer) error {
			body, totalSize, err := env.Fetcher.Get(tctx, rawurl)
			if err != nil {
				return fmt.Errorf("fetch %s: %s", rawurl, err)
			}
			defer body.Close()
			// 嗅探窗：二进制即拒（中止下载，不写日志）
			head := make([]byte, curlSniffBytes)
			n, _ := io.ReadFull(io.LimitReader(body, curlSniffBytes), head)
			head = head[:n]
			if !isTextContent(head) {
				return &binaryOutputError{mime: detectMIME(head, rawurl)}
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
