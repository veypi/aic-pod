package hostfs

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// RenamePinned atomically replaces a directory entry between already pinned
// parents. The caller owns authorization; this primitive never resolves an
// absolute pathname or follows the source/destination leaf.
func RenamePinned(from *os.Root, src string, to *os.Root, dst string) error {
	for _, name := range []string{src, dst} {
		if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\x00") || strings.ContainsRune(name, filepath.Separator) {
			return fmt.Errorf("rename requires single path components")
		}
	}
	a, err := from.Open(".")
	if err != nil {
		return err
	}
	defer a.Close()
	b, err := to.Open(".")
	if err != nil {
		return err
	}
	defer b.Close()
	return renameReplace(int(a.Fd()), src, int(b.Fd()), dst)
}
