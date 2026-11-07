package fsx

import (
	"context"
	"path"
	"regexp/syntax"
	"runtime"
	"strings"
	"sync"

	"github.com/veypi/vsh/commands"
)

// rgIndexLiteral extracts a required literal without changing match semantics.
// The indexed paths are only candidates; rgFileRows still reads current content.
func rgIndexLiteral(pattern string) (string, bool) {
	re, err := syntax.Parse(pattern, syntax.Perl)
	if err != nil {
		return "", false
	}
	literal := requiredFSLiteral(re)
	return literal, len([]rune(literal)) >= 3
}

func requiredFSLiteral(re *syntax.Regexp) string {
	if re == nil {
		return ""
	}
	switch re.Op {
	case syntax.OpLiteral:
		return string(re.Rune)
	case syntax.OpCapture:
		return requiredFSLiteral(re.Sub[0])
	case syntax.OpPlus:
		return requiredFSLiteral(re.Sub[0])
	case syntax.OpRepeat:
		if re.Min > 0 {
			return requiredFSLiteral(re.Sub[0])
		}
	case syntax.OpConcat:
		best := ""
		for _, sub := range re.Sub {
			if literal := requiredFSLiteral(sub); len(literal) > len(best) {
				best = literal
			}
		}
		return best
	}
	return ""
}

func rgIndexedContent(ctx context.Context, env *Env, root, pattern string, globs []string, hidden bool, depth *int) (rgFileIter, bool) {
	if env.FileIndex == nil {
		return nil, false
	}
	literal, ok := rgIndexLiteral(pattern)
	if !ok {
		return nil, false
	}
	candidates, ok := env.FileIndex.Candidates(ctx, commands.FileIndexQuery{Root: root, Literals: []string{literal}, IncludeHidden: hidden})
	if !ok {
		return nil, false
	}
	return rgIndexedIterator(env, root, candidates, globs, hidden, rgDepthValue(depth)), true
}

func rgIndexedFiles(ctx context.Context, env *Env, root string, globs []string, hidden bool, depth int) (rgFileIter, bool) {
	if env.FileIndex == nil {
		return nil, false
	}
	candidates, ok := env.FileIndex.Candidates(ctx, commands.FileIndexQuery{Root: root, FilesOnly: true, IncludeHidden: hidden})
	if !ok {
		return nil, false
	}
	return rgIndexedIterator(env, root, candidates, globs, hidden, depth), true
}

func rgDepthValue(depth *int) int {
	if depth == nil {
		return 0
	}
	return *depth
}

func rgIndexedIterator(env *Env, root string, candidates, globs []string, hidden bool, depth int) rgFileIter {
	return func(yield func(string) bool) error {
		filtered := make([]string, 0, len(candidates))
		for _, name := range candidates {
			rel, ok := rgFSRelative(root, name)
			if !ok || rel == "." || !rgFSDepthAllowed(rel, depth) || rgFSSkipped(rel, hidden) {
				continue
			}
			if !globOK(globs, path.Base(name)) {
				continue
			}
			if err := env.CheckPolicy("fs rg", name, false); err != nil {
				continue
			}
			filtered = append(filtered, name)
		}

		valid := make([]bool, len(filtered))
		jobs := make(chan int)
		var wg sync.WaitGroup
		for range min(8, len(filtered)) {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := range jobs {
					info, err := env.FS.Stat(filtered[i])
					valid[i] = err == nil && !info.IsDir()
				}
			}()
		}
		for i := range filtered {
			jobs <- i
		}
		close(jobs)
		wg.Wait()
		for i, name := range filtered {
			if valid[i] && !yield(name) {
				return errRgStop
			}
		}
		return nil
	}
}

func rgFSRelative(root, name string) (string, bool) {
	root = path.Clean(root)
	name = path.Clean(name)
	if root == name || (runtime.GOOS == "windows" && strings.EqualFold(root, name)) {
		return ".", true
	}
	prefix := strings.TrimSuffix(root, "/") + "/"
	if len(name) < len(prefix) {
		return "", false
	}
	if !strings.HasPrefix(name, prefix) && (runtime.GOOS != "windows" || !strings.EqualFold(name[:len(prefix)], prefix)) {
		return "", false
	}
	return name[len(prefix):], true
}

func rgFSDepthAllowed(rel string, depth int) bool {
	if depth <= 0 {
		return true
	}
	dir := path.Dir(rel)
	if dir == "." {
		return true
	}
	return len(strings.Split(dir, "/")) < depth
}

func rgFSSkipped(rel string, hidden bool) bool {
	components := strings.Split(rel, "/")
	for i, component := range components {
		if i < len(components)-1 && skipDirs[component] {
			return true
		}
		if !hidden && strings.HasPrefix(component, ".") {
			return true
		}
	}
	return false
}
