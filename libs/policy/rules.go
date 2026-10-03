// Package policy provides product command rules and delegates filesystem and
// network rule parsing to vbox. It owns no configuration or approval state.
package policy

import (
	"fmt"
	"github.com/veypi/vbox"
	"strings"
)

const (
	EffectDeny = "deny"
	EffectRO   = "ro"
	EffectRW   = "rw"
)

func ParseFSRule(raw string) (string, string, error) {
	effect, pattern, err := vbox.ParseFSRule(raw)
	return effect.String(), pattern, err
}
func ValidateFSRules(rows []string) error             { return vbox.ValidateFSRules(rows) }
func ValidateFSGrantTarget(target string) error       { return vbox.ValidateFSGrantTarget(target) }
func ParseTargetRule(raw string) (bool, Entry, error) { return vbox.ParseTargetRule(raw) }
func ValidateTargetRules(rows []string) error         { return vbox.ValidateTargetRules(rows) }

// ParseExecRule accepts one exact name. Whole-domain behavior belongs to policy.
func ParseExecRule(raw string) (allow bool, name string, err error) {
	effect, name, ok := strings.Cut(raw, ":")
	if !ok || (effect != "allow" && effect != "deny") || ValidateCommandName(name) != nil {
		return false, "", fmt.Errorf("invalid exec rule %q (want allow:name or deny:name)", raw)
	}
	return effect == "allow", name, nil
}
func ValidateCommandName(name string) error {
	if name == "" || strings.TrimSpace(name) != name || strings.ContainsAny(name, "/\\ \t\r\n\x00*?") {
		return fmt.Errorf("invalid exec command %q", name)
	}
	return nil
}
func ValidateExecRules(rows []string) error {
	for _, raw := range rows {
		if _, _, err := ParseExecRule(raw); err != nil {
			return err
		}
	}
	return nil
}
func CommandAllowed(mode string, rows []string, name string) bool {
	if (mode != "open" && mode != "deny") || ValidateCommandName(name) != nil {
		return false
	}
	for _, raw := range rows {
		allow, target, err := ParseExecRule(raw)
		if err != nil {
			return false
		}
		if target == name {
			return allow
		}
	}
	return mode == "open"
}
