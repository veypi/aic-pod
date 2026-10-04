package host

// Configuration is parsed as data. Never run ssh -G against ambient config:
// Match exec is evaluated even when no connection is made.
import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"
)

const sshConfigLimit = 1 << 20

type sshTarget struct {
	Original, Host, User string
	Port                 int
	Identities           []string
	IdentitiesOnly       bool
	Home                 string
}

func (t sshTarget) address() string { return net.JoinHostPort(t.Host, strconv.Itoa(t.Port)) }

func parseSSHTarget(raw string) (sshTarget, error) {
	t := sshTarget{Original: raw}
	if i := strings.LastIndexByte(raw, '@'); i >= 0 {
		t.User, raw = raw[:i], raw[i+1:]
		if !sshUserValid(t.User) {
			return t, fmt.Errorf("invalid SSH user")
		}
	}
	if strings.HasPrefix(raw, "[") {
		i := strings.IndexByte(raw, ']')
		if i < 0 {
			return t, fmt.Errorf("IPv6 target requires [address][:port]")
		}
		t.Host = raw[1:i]
		if i+1 < len(raw) {
			if raw[i+1] != ':' {
				return t, fmt.Errorf("invalid SSH target")
			}
			p, err := sshPort(raw[i+2:])
			if err != nil {
				return t, err
			}
			t.Port = p
		}
	} else {
		if strings.Count(raw, ":") > 1 {
			return t, fmt.Errorf("IPv6 target requires [address][:port]")
		}
		t.Host = raw
		if i := strings.IndexByte(raw, ':'); i >= 0 {
			t.Host = raw[:i]
			p, err := sshPort(raw[i+1:])
			if err != nil {
				return t, err
			}
			t.Port = p
		}
	}
	if !sshHostValid(t.Host) {
		return t, fmt.Errorf("invalid SSH host %q", t.Host)
	}
	return t, nil
}

func sshPort(s string) (int, error) {
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("invalid SSH port %q", s)
		}
	}
	p, err := strconv.Atoi(s)
	if err != nil || p < 1 || p > 65535 {
		return 0, fmt.Errorf("invalid SSH port %q", s)
	}
	return p, nil
}

// defaultSSHUser 取当前 OS 用户的默认 SSH 用户名：Windows 的 user.Current()
// 返回 "DOMAIN\user"（域或本地机器名作前缀），而 ssh 需要裸用户名——否则
// `ssh somehost` 会拿域账号去连（2026-10-05 win 实机：提示 user DESKTOP-6QGJPLH\v）。
// 其他平台用户名不含反斜杠，本函数无副作用。
func defaultSSHUser(name string) string {
	if i := strings.LastIndexByte(name, '\\'); i >= 0 {
		return name[i+1:]
	}
	return name
}

func sshUserValid(s string) bool {
	if s == "" || strings.HasPrefix(s, "-") {
		return false
	}
	for _, c := range s {
		if !strings.ContainsRune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789._-@\\", c) {
			return false
		}
	}
	return true
}

func sshHostValid(s string) bool {
	if _, err := netip.ParseAddr(s); err == nil {
		return true
	}
	if s == "" || len(s) > 253 || strings.HasPrefix(s, "-") {
		return false
	}
	for _, c := range s {
		if !strings.ContainsRune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789.-_", c) {
			return false
		}
	}
	return strings.Trim(s, ".") != ""
}

// sshWords is a deliberately small, non-expanding lexer shared by static
// config and our batch language. It never executes substitutions or a shell.
func sshWords(s string) ([]string, error) {
	var words []string
	var b strings.Builder
	var quote rune
	escape, started := false, false
	flush := func() {
		if started {
			words = append(words, b.String())
			b.Reset()
			started = false
		}
	}
	for _, c := range s {
		if c == 0 || (unicode.IsControl(c) && c != '\t') {
			return nil, fmt.Errorf("control character in input")
		}
		if escape {
			b.WriteRune(c)
			escape = false
			started = true
			continue
		}
		if c == '\\' && quote != '\'' {
			escape = true
			started = true
			continue
		}
		if quote != 0 {
			if c == quote {
				quote = 0
			} else {
				b.WriteRune(c)
			}
			continue
		}
		if c == '\'' || c == '"' {
			quote = c
			started = true
			continue
		}
		if c == '#' {
			break
		}
		if unicode.IsSpace(c) {
			flush()
			continue
		}
		started = true
		b.WriteRune(c)
	}
	if escape || quote != 0 {
		return nil, fmt.Errorf("unterminated quote or escape")
	}
	flush()
	return words, nil
}

type sshConfigReader struct {
	target      *sshTarget
	alias, root string
	active      bool
	seen        map[string]bool
	bytes       int64
	stack       map[string]bool
}

func resolveSSHTarget(raw, home string, port int) (sshTarget, error) {
	t, err := parseSSHTarget(raw)
	if err != nil {
		return t, err
	}
	t.Home = home
	if port != 0 {
		if t.Port != 0 && t.Port != port {
			return t, fmt.Errorf("conflicting SSH ports")
		}
		t.Port = port
	}
	// An explicit device profile replaces, rather than merges with, ambient
	// OpenSSH config. This permits static profiles for devices using Match/Jump.
	config := filepath.Join(home, ".aic", "ssh_config")
	if _, err := os.Stat(config); os.IsNotExist(err) {
		config = filepath.Join(home, ".ssh", "config")
	} else if err != nil {
		return t, err
	}
	r := sshConfigReader{target: &t, alias: strings.ToLower(t.Host), root: filepath.Dir(config), active: true,
		seen: map[string]bool{"user": t.User != "", "port": t.Port != 0}, stack: map[string]bool{}}
	if _, err := os.Stat(config); err == nil {
		if err := r.read(config, 0); err != nil {
			return t, err
		}
	} else if !os.IsNotExist(err) {
		return t, err
	}
	if t.Port == 0 {
		t.Port = 22
	}
	if t.User == "" {
		u, err := user.Current()
		if err != nil {
			return t, err
		}
		t.User = defaultSSHUser(u.Username)
	}
	if !sshHostValid(t.Host) || !sshUserValid(t.User) {
		return t, fmt.Errorf("invalid resolved SSH host/user")
	}
	t.Host = strings.TrimSuffix(strings.ToLower(t.Host), ".")
	if ip, err := netip.ParseAddr(t.Host); err == nil {
		t.Host = ip.String()
	}
	for i, name := range t.Identities {
		name, err = expandSSHIdentity(name, t)
		if err != nil {
			return t, err
		}
		t.Identities[i] = name
	}
	return t, nil
}

func (r *sshConfigReader) read(name string, depth int) error {
	if depth > 8 {
		return fmt.Errorf("SSH Include depth exceeds 8")
	}
	real, err := filepath.EvalSymlinks(name)
	if err != nil {
		return err
	}
	// Ambient config explicitly delegates to static files selected by the device
	// owner (e.g. ~/.orbstack/ssh/config). Restricting it to ~/.ssh breaks such
	// configurations before any Host block can be selected. Device profiles
	// retain their narrower boundary to exclude mutable sessions/device state.
	if depth > 0 && filepath.Base(r.root) == ".aic" {
		root, err := filepath.EvalSymlinks(r.root)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, real)
		if err != nil || !strings.HasPrefix(rel, "ssh"+string(filepath.Separator)) {
			return fmt.Errorf("device SSH Include must be inside ~/.aic/ssh (not sessions or other device state)")
		}
	}
	if r.stack[real] {
		return fmt.Errorf("SSH Include cycle")
	}
	r.stack[real] = true
	defer delete(r.stack, real)
	info, err := os.Stat(real)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("SSH configuration must be a regular file")
	}
	f, err := os.Open(real)
	if err != nil {
		return err
	}
	defer f.Close()
	s := bufio.NewScanner(io.LimitReader(f, sshConfigLimit+1))
	s.Buffer(make([]byte, 4096), sshConfigLimit+1)
	line := 0
	for s.Scan() {
		line++
		text := s.Text()
		r.bytes += int64(len(text) + 1)
		if r.bytes > sshConfigLimit {
			return fmt.Errorf("SSH config exceeds 1 MiB")
		}
		// OpenSSH permits both Keyword value and Keyword=value.
		trim := strings.TrimSpace(text)
		if i := strings.IndexAny(trim, "= \t"); i >= 0 {
			trim = trim[:i] + " " + strings.TrimLeft(trim[i:], "= \t")
		}
		w, e := sshWords(trim)
		if e != nil {
			return fmt.Errorf("%s:%d: %w", name, line, e)
		}
		if len(w) == 0 {
			continue
		}
		key := strings.ToLower(w[0])
		values := w[1:]
		bad := func() error {
			return fmt.Errorf("%s:%d: unsupported static SSH directive %s; use ~/.aic/ssh_config", name, line, w[0])
		}
		if key == "match" {
			return bad()
		} // even inactive Match changes subsequent scope
		if key == "host" {
			if len(values) == 0 {
				return bad()
			}
			r.active = false
			for _, p := range values {
				neg := strings.HasPrefix(p, "!")
				p = strings.TrimPrefix(p, "!")
				if strings.ContainsAny(p, "[]/\\") {
					return bad()
				}
				ok, e := filepath.Match(strings.ToLower(p), r.alias)
				if e != nil {
					return bad()
				}
				if ok && neg {
					r.active = false
					break
				}
				if ok {
					r.active = true
				}
			}
			continue
		}
		if !r.active {
			continue
		}
		if key == "include" {
			if len(values) == 0 {
				return fmt.Errorf("%s:%d: SSH Include requires a static file path", name, line)
			}
			for _, file := range values {
				expanded, err := r.includePath(file)
				if err == nil {
					err = r.read(expanded, depth+1)
				}
				if err != nil {
					return fmt.Errorf("%s:%d: SSH Include target %q: %w", name, line, file, err)
				}
			}
			continue
		}
		if len(values) != 1 {
			return bad()
		}
		value := values[0]
		switch key {
		case "hostname", "user", "port", "identitiesonly":
			if r.seen[key] {
				continue
			}
			r.seen[key] = true
			switch key {
			case "hostname":
				r.target.Host = value
			case "user":
				r.target.User = value
			case "port":
				p, err := sshPort(value)
				if err != nil {
					return err
				}
				r.target.Port = p
			case "identitiesonly":
				if value != "yes" && value != "no" {
					return bad()
				}
				r.target.IdentitiesOnly = value == "yes"
			}
		case "identityfile":
			if len(r.target.Identities) >= 32 {
				return fmt.Errorf("too many SSH identities")
			}
			r.target.Identities = append(r.target.Identities, value)
		default:
			return bad()
		}
	}
	return s.Err()
}

func (r *sshConfigReader) includePath(name string) (string, error) {
	if name == "" || strings.ContainsAny(name, "*?[]$%") {
		return "", fmt.Errorf("expected a static path; glob, environment and token expansion are unsupported")
	}
	if strings.HasPrefix(name, "~/") {
		name = filepath.Join(r.target.Home, name[2:])
	} else if strings.HasPrefix(name, "~") {
		return "", fmt.Errorf("only ~/ is supported for home expansion")
	}
	if !filepath.IsAbs(name) {
		// OpenSSH resolves relative includes against ~/.ssh, including nested
		// ones, rather than against the including file's directory.
		name = filepath.Join(r.root, name)
	}
	return filepath.Clean(name), nil
}

func expandSSHIdentity(s string, t sshTarget) (string, error) {
	if s == "none" {
		return s, nil
	}
	if strings.HasPrefix(s, "~/") {
		s = filepath.Join(t.Home, s[2:])
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '%' {
			b.WriteByte(s[i])
			continue
		}
		i++
		if i == len(s) {
			return "", fmt.Errorf("invalid identity token")
		}
		switch s[i] {
		case '%':
			b.WriteByte('%')
		case 'd':
			b.WriteString(t.Home)
		case 'h':
			b.WriteString(t.Host)
		case 'r':
			b.WriteString(t.User)
		case 'p':
			b.WriteString(strconv.Itoa(t.Port))
		default:
			return "", fmt.Errorf("unsupported identity token %%%c", s[i])
		}
	}
	s = b.String()
	if !filepath.IsAbs(s) || strings.ContainsAny(s, "\x00\r\n$") {
		return "", fmt.Errorf("IdentityFile must be an absolute static path")
	}
	return filepath.Clean(s), nil
}
