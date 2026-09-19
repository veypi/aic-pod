// Package fs defines structured filesystem locations and results, independent
// from AI text formatting and physical host path syntax.
package fs

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/veypi/aic-pod/protocol/hosts"
)

const Contract = "fs/1"

type Path struct {
	RootID   string   `json:"root_id"`
	Segments []string `json:"segments"`
}

func (p Path) Validate(windows bool) error {
	if !hosts.ValidID(p.RootID) || p.Segments == nil || len(p.Segments) > 256 {
		return hosts.Fail("invalid_argument", "Invalid file location")
	}
	total := 0
	for _, s := range p.Segments {
		total += len(s)
		if s == "" || s == "." || s == ".." || !utf8.ValidString(s) || strings.ContainsAny(s, "/\x00") || windows && strings.ContainsAny(s, `\:`) || windows && (strings.HasSuffix(s, ".") || strings.HasSuffix(s, " ")) {
			return hosts.Fail("invalid_argument", "Invalid path segment")
		}
	}
	if total > 16384 {
		return hosts.Fail("invalid_argument", "Path exceeds limit")
	}
	return nil
}

type Entry struct {
	Path       Path   `json:"path"`
	Name       string `json:"name"`
	Kind       string `json:"kind"`
	Size       *int64 `json:"size,omitempty"`
	ModifiedAt string `json:"modified_at,omitempty"`
	Version    string `json:"version"`
	MediaType  string `json:"media_type,omitempty"`
}
type Condition struct {
	Absent  bool   `json:"absent,omitempty"`
	Version string `json:"version,omitempty"`
	Any     bool   `json:"any,omitempty"`
}

func (c Condition) Validate() error {
	n := 0
	if c.Absent {
		n++
	}
	if c.Version != "" {
		n++
	}
	if c.Any {
		n++
	}
	if n != 1 {
		return hosts.Fail("invalid_argument", "Exactly one write condition is required")
	}
	return nil
}
func (c Condition) Check(version string, exists bool) error {
	if err := c.Validate(); err != nil {
		return err
	}
	if c.Absent && exists {
		return hosts.Fail("already_exists", "Destination already exists")
	}
	if c.Version != "" && (!exists || c.Version != version) {
		f := hosts.Fail("version_conflict", "File changed since it was read")
		f.Details = map[string]any{"current_version": version}
		return f
	}
	return nil
}
func (p Path) String() string { return fmt.Sprintf("%s/%s", p.RootID, strings.Join(p.Segments, "/")) }
