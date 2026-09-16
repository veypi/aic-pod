//
// sysca.go
// Copyright (C) 2026 veypi <i@veypi.com>
// Distributed under terms of the MIT license.
//

package fsauth

import (
	"path/filepath"
)

// SystemCAReadPatterns supplies read-only CA resources to both fs checks and
// process sandboxes. Explicit deny still wins; these rules grant no writes.
// Both canonical and literal paths cover system symlink spellings.
func SystemCAReadPatterns() []string {
	pats := systemCAPaths()
	out := make([]string, 0, len(pats)*2)
	seen := map[string]bool{}
	add := func(s string) {
		if s == "" || seen[s] {
			return
		}
		seen[s] = true
		out = append(out, s)
	}
	for _, pat := range pats {
		e, ok := expandVars(pat)
		if !ok {
			continue
		}
		add(canonicalPattern(e))
		add(filepath.ToSlash(e))
	}
	return out
}
