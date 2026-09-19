//go:build linux

package hostfs

import "golang.org/x/sys/unix"

func renameNoReplace(from int, src string, to int, dst string) error {
	return unix.Renameat2(from, src, to, dst, unix.RENAME_NOREPLACE)
}

func renameReplace(from int, src string, to int, dst string) error {
	return unix.Renameat(from, src, to, dst)
}
