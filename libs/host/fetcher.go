package host

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"time"

	"github.com/veypi/aic-pod/libs/exec_procs"
	"github.com/veypi/aic-pod/libs/proto"
	"github.com/veypi/aic-pod/libs/vcore"
)

// shellCurlFetcher 是物理 host 的 curl fetcher（2026-09-06/07 用户决策）：
// http(s) 地址走真 curl 二进制 + 统一沙箱（exec_procs.Spawn）；进程内
// net/http 形态废除（进程内 fetch 不受沙箱网络管控，deny 锁定会形同虚设）。
//
// 管控双层：工具层目标闸（Fetch 前置 netauth.Allowed，host:port 精判）+
// 内核层沙箱（net_policy=deny 锁定模式时 localhost-only + *:port 粗放行；
// 默认 open 零网络规则）。已知缺口：-L 重定向跳由 curl 进程内部跟随，
// deny 模式下已放行端口的跨主机跳转内核不可拦（文档化缺口）。
//
// 语义对齐原 httpFetcher：-sS 静默 + -L 跟随重定向；HTTP 状态码不报错
// （body 原样透传，与 net/http 一致）；连接/解析失败经 Body EOF 处的
// 非零退出错误上报（vcore 侧 io.Copy 感知）。
// -q 禁读 ~/.curlrc（沙箱 deny 表之外的双保险）；body 经 stdin
// --data-binary @- 传入（curl 不接触文件系统取数据）。
// 沙箱 profile = read-only（curl 不落盘，-o 语义由 vcore 经 VFS 承担）+
// 三域授权快照（deny 表防 curl 读取 deny 路径，net 快照给出站闸）。
type shellCurlFetcher struct {
	c   *Client
	sid string
}

// buildCurlArgv 由 vcore.HTTPReq 构造 curl argv（纯函数，测试直接引用）。
// -q 禁读 ~/.curlrc；-sS 静默；-L 跟随重定向；--proto-redir 钉住重定向协议族
// （http/https，302 到 file:// 等 scheme 不跟）；body 经 stdin 传入（@-）；
// resolveIPs 非空时每 IP 追加 --resolve host:port:ip（deny 锁定模式沙箱内无
// DNS——pod 侧预解析钉住，兼防 rebinding）。
func buildCurlArgv(req vcore.HTTPReq, host string, port int, resolveIPs []string) []string {
	argv := []string{"curl", "-q", "-sS", "-L", "--proto-redir", "=http,https", "-X", req.Method}
	for _, ip := range resolveIPs {
		argv = append(argv, "--resolve", fmt.Sprintf("%s:%d:%s", host, port, ip))
	}
	for k, v := range req.Headers {
		argv = append(argv, "-H", k+": "+v)
	}
	if len(req.Body) > 0 {
		argv = append(argv, "--data-binary", "@-")
	}
	return append(argv, req.URL) // URL 已经 vcore scheme 校验（http/https），不可能以 - 开头
}

// Fetch 实现 vcore.Fetcher。totalSize 恒 -1（子进程流无法预知长度；
// vcore 的 LimitReader 限额不受影响）。
//
// 目标闸（工具层精判，两模式皆生效）：spawn 前 netPol.Allowed 校验——
// net_policy=open 时拦 net_deny；deny 锁定模式时要求 net_allow 命中
// （内核 *:port 粗放行之上的 host:port 精细层，也是 deny 模式下的主出站点）。
func (f shellCurlFetcher) Fetch(ctx context.Context, req vcore.HTTPReq) (io.ReadCloser, int64, error) {
	host, port, err := urlHostPort(req.URL)
	if err != nil {
		return nil, -1, err
	}
	if !f.c.netPol.Allowed(f.sid, host, port) {
		return nil, -1, fmt.Errorf("curl: target %s:%d is not in the net allow list (or hit net_deny) — request access via: grant net %s:%d [--temp|--permanent]", host, port, host, port)
	}
	// deny 锁定模式：沙箱内无 DNS（2026-09-07 实测 mDNSResponder/dnssd 多形态
	// 放行均不生效）——FQDN 目标在 pod 侧预解析并 --resolve 钉住。
	var resolveIPs []string
	if !f.c.netPol.OpenMode() {
		if _, err := netip.ParseAddr(host); err != nil {
			resolveIPs = resolveHostIPs(ctx, host)
			if len(resolveIPs) == 0 {
				return nil, -1, fmt.Errorf("curl: cannot resolve %q on host side (required in net_policy=deny): check DNS or grant an IP target", host)
			}
		}
	}
	argv := buildCurlArgv(req, host, port, resolveIPs)

	netDeny, netAllow := f.c.netPol.Snapshot(f.sid)
	sp, err := f.c.procs.Spawn(ctx, exec_procs.StartOptions{
		Command:   "curl " + req.URL,
		Exec:      argv,
		Level:     proto.LevelRead, // curl 进程自身零写需求；落盘由 vcore VFS 承担
		DenyPaths: f.c.policy.DenyPatterns(),
		FsOpen:    f.c.policy.OpenMode(),
		NetOpen:   f.c.netPol.OpenMode(),
		NetDeny:   netDeny,
		NetAllow:  netAllow,
	})
	if err != nil {
		return nil, -1, err
	}
	if len(req.Body) > 0 {
		go func() {
			_, _ = sp.Stdin.Write(req.Body)
			_ = sp.Stdin.Close()
		}()
	}
	return sp.Body(), -1, nil
}

// urlHostPort 解析 URL 的生效 host/port（缺省端口按 scheme：http=80 https=443）。
// URL 已经 vcore scheme 校验（仅 http/https）。
func urlHostPort(rawurl string) (string, int, error) {
	u, err := url.Parse(rawurl)
	if err != nil || u.Hostname() == "" {
		return "", 0, fmt.Errorf("curl: invalid url %q", rawurl)
	}
	port := 443
	if u.Scheme == "http" {
		port = 80
	}
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			return "", 0, fmt.Errorf("curl: invalid port in %q", rawurl)
		}
		port = n
	}
	return u.Hostname(), port, nil
}

// resolveHostIPs 在 pod 进程内预解析 FQDN（3s 超时 best-effort；锁定模式补偿，
// 见 Fetch）。最多返回 4 个地址。
func resolveHostIPs(ctx context.Context, host string) []string {
	rctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	ips, err := net.DefaultResolver.LookupIP(rctx, "ip", host)
	if err != nil {
		return nil
	}
	out := make([]string, 0, 4)
	for _, ip := range ips {
		out = append(out, ip.String())
		if len(out) >= 4 {
			break
		}
	}
	return out
}
