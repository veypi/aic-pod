//go:build darwin

package hostfs

import "golang.org/x/sys/unix"

func renameNoReplace(from int, src string, to int, dst string) error {
	return unix.RenameatxNp(from, src, to, dst, unix.RENAME_EXCL)
}

func renameReplace(from int, src string, to int, dst string) error {
	return unix.Renameat(from, src, to, dst)
}
