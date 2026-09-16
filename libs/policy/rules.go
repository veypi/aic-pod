// Package policy defines the common execution-policy primitives. It has no
// session approval state and cannot elevate a call.
package policy

import (
	"fmt"
	"strings"
)

type FSAllow struct {
	Path     string
	ReadOnly bool
}

func ParseFSAllow(raw string) (FSAllow, error) {
	item := FSAllow{Path: raw}
	if strings.HasPrefix(raw, "ro:") {
		item.Path = strings.TrimPrefix(raw, "ro:")
		item.ReadOnly = true
	}
	if strings.TrimSpace(item.Path) == "" || strings.ContainsRune(item.Path, 0) {
		return FSAllow{}, fmt.Errorf("invalid fs_allow entry %q", raw)
	}
	return item, nil
}
func ValidateFS(entries []string, allow bool) error {
	for _, raw := range entries {
		if allow {
			if _, err := ParseFSAllow(raw); err != nil {
				return err
			}
		} else if strings.TrimSpace(raw) == "" || strings.HasPrefix(raw, "ro:") || strings.ContainsRune(raw, 0) {
			return fmt.Errorf("invalid fs_deny entry %q", raw)
		}
	}
	return nil
}
func ValidateExec(entries []string) error {
	for _, name := range entries {
		if name == "" || strings.TrimSpace(name) != name || strings.ContainsAny(name, "/\\ \t\r\n\x00") || (name != "*" && strings.ContainsAny(name, "*?")) {
			return fmt.Errorf("invalid exec command %q", name)
		}
	}
	return nil
}
func CommandAllowed(mode string, deny, allow []string, name string) bool {
	hit := func(list []string) bool {
		for _, p := range list {
			if p == "*" || p == name {
				return true
			}
		}
		return false
	}
	if mode != "open" && mode != "deny" {
		return false
	}
	return !hit(deny) && (hit(allow) || mode == "open")
}
