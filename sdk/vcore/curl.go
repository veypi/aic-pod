package vcore

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// ---- curl（§5.4）----

// curl -o <path> <url> [--max-size <MB>]：仅 http/https，GET 流式写入；
// 目标已存在报错不覆盖；超限中止并删除半成品文件。
// SSRF 防护由 Fetcher 实现方注入（cloud 严格 / 物理 host 不限制）。
func cmdCurl(ctx context.Context, env *Env, argv []string) (*Result, error) {
	pa, err := parseArgv("curl", argvSpec{
		values: map[string]bool{"-o": true, "--max-size": true},
		minPos: 1, maxPos: 1,
	}, argv)
	if err != nil {
		return nil, err
	}
	dst := pa.values["-o"]
	if dst == "" {
		return nil, execErr("curl", "-o is required")
	}
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

	abs, err := env.Resolve(dst)
	if err != nil {
		return nil, execErr("curl", "%s", err)
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

	if err := env.VFS.MkdirAll(dirOf(abs)); err != nil {
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
