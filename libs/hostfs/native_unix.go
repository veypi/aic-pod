//go:build darwin || linux

package hostfs

import (
	"fmt"
	"io/fs"
	"os"
	"reflect"
	"syscall"
)

func atomicReplaceSupported() bool { return true }
func safeReadSupported() bool      { return true }
func openRegular(root *os.Root, name string) (*os.File, error) {
	return root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
}
func fileIdentity(info fs.FileInfo) string {
	// Include object identity and change time, but not access time (reads change it).
	s := reflect.Indirect(reflect.ValueOf(info.Sys()))
	if s.Kind() != reflect.Struct {
		return ""
	}
	return fmt.Sprint(s.FieldByName("Dev"), "/", s.FieldByName("Ino"), "/", s.FieldByName("Ctim"), "/", s.FieldByName("Ctimespec"))
}

// unix 无「隐藏属性」概念：隐藏口径按点开头（由调用方处理）。
func entryHidden(fs.DirEntry) bool { return false }
func dirLink(fs.FileInfo) bool     { return false }
