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
