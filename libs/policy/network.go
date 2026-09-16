package policy

import (
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"unicode"
)

// Entry 是归一化目标条目（Host 小写/IP 归一，Port 数字串或 *）。
type Entry struct {
	Host string
	Port string
}

// String 返回规范形态 host:port。
func (e Entry) String() string { return net.JoinHostPort(e.Host, e.Port) }

// ParseEntry 解析并归一化目标条目：bare host → host:*；剥 user@ 前缀；
// host 小写/去尾点、IP 经 netip 归一；port 须为 1-65535 或 *。
// IPv6 须带括号（[::1]:22）；不带括号的多冒号形态按 bare host 处理（port=*）。
func ParseEntry(s string) (Entry, error) {
	s = strings.TrimSpace(s)
	if i := strings.LastIndex(s, "@"); i >= 0 {
		s = s[i+1:] // 剥 user@（ssh 目标形态 user@host:port）
	}
	if s == "" {
		return Entry{}, fmt.Errorf("empty target")
	}
	if strings.IndexFunc(s, func(r rune) bool { return r == '/' || unicode.IsSpace(r) }) >= 0 {
		return Entry{}, fmt.Errorf("invalid target %q: want host[:port]", s)
	}
	host, port := "", ""
	if strings.HasPrefix(s, "[") {
		h, p, err := net.SplitHostPort(s)
		if err != nil {
			return Entry{}, fmt.Errorf("invalid target %q: %v", s, err)
		}
		host, port = h, p
	} else if strings.Count(s, ":") == 1 {
		h, p, err := net.SplitHostPort(s)
		if err != nil {
			return Entry{}, fmt.Errorf("invalid target %q: %v", s, err)
		}
		host, port = h, p
	} else if strings.Count(s, ":") == 0 {
		host, port = s, "*"
	} else {
		host, port = s, "*" // 不带括号的 bare IPv6
	}
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if ip, err := netip.ParseAddr(host); err == nil {
		host = ip.String()
	}
	if host == "" {
		return Entry{}, fmt.Errorf("invalid target %q: empty host", s)
	}
	if strings.Contains(host, "*") {
		// 通配主机整条拒绝：entryMatch 是归一后字符串相等，* 条目恒惰性
		//（永不匹配）——静默接受会让用户以为生效（set_config 显式校验语义）。
		return Entry{}, fmt.Errorf("invalid target %q: wildcard host is not supported (entries match exact hosts only)", s)
	}
	if port != "*" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return Entry{}, fmt.Errorf("invalid port %q in %q: want 1-65535 or *", port, s)
		}
		port = strconv.Itoa(n)
	}
	return Entry{Host: host, Port: port}, nil
}

// ValidateEntries 批量校验条目合法性（set_config 显式校验用；首个非法条目即报错）。
func ValidateEntries(list []string) error {
	for _, s := range list {
		if _, err := ParseEntry(s); err != nil {
			return err
		}
	}
	return nil
}
