//go:build windows

package hostfs

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"

	hosts "github.com/veypi/aic-pod/protocol/fs"
	"golang.org/x/sys/windows"
)

func atomicReplaceSupported() bool { return true }
func safeReadSupported() bool      { return true }

// Open relative to the pinned parent handle, never by reconstructing an absolute
// path. FILE_OPEN_REPARSE_POINT opens the leaf itself instead of following it.
// https://learn.microsoft.com/en-us/windows/win32/api/winternl/nf-winternl-ntcreatefile
func openRelative(parent windows.Handle, name string, access, options uint32) (windows.Handle, error) {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\:`) {
		return windows.InvalidHandle, syscall.EINVAL
	}
	objectName, err := windows.NewNTUnicodeString(name)
	if err != nil {
		return windows.InvalidHandle, err
	}
	attrs := windows.OBJECT_ATTRIBUTES{RootDirectory: parent, ObjectName: objectName, Attributes: windows.OBJ_CASE_INSENSITIVE}
	attrs.Length = uint32(unsafe.Sizeof(attrs))
	var handle windows.Handle
	err = windows.NtCreateFile(&handle, access|windows.SYNCHRONIZE, &attrs, &windows.IO_STATUS_BLOCK{}, nil,
		windows.FILE_ATTRIBUTE_NORMAL, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		windows.FILE_OPEN, options|windows.FILE_OPEN_REPARSE_POINT|windows.FILE_SYNCHRONOUS_IO_NONALERT, 0, 0)
	return handle, windowsFileError(err)
}

func windowsFileError(err error) error {
	if status, ok := err.(windows.NTStatus); ok {
		err = status.Errno()
	}
	if err == windows.ERROR_NOT_SAME_DEVICE {
		return syscall.EXDEV
	}
	return err
}

func openRegular(root *os.Root, name string) (*os.File, error) {
	dir, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	handle, err := openRelative(windows.Handle(dir.Fd()), name, windows.FILE_GENERIC_READ, windows.FILE_NON_DIRECTORY_FILE)
	if err != nil {
		return nil, err
	}
	var info windows.ByHandleFileInformation
	err = windows.GetFileInformationByHandle(handle, &info)
	if err != nil || info.FileAttributes&(windows.FILE_ATTRIBUTE_REPARSE_POINT|windows.FILE_ATTRIBUTE_DIRECTORY) != 0 {
		windows.CloseHandle(handle)
		if err != nil {
			return nil, err
		}
		return nil, hosts.Fail("unsupported", "Read requires a regular file, not a reparse point")
	}
	return os.NewFile(uintptr(handle), filepath.Join(root.Name(), name)), nil
}

func fileIdentity(info fs.FileInfo) string {
	// FileInfo.Sys exposes creation time and attributes on Windows. Keep access
	// time out of versions; reads change it. Open/read also verify os.SameFile.
	if data, ok := info.Sys().(*syscall.Win32FileAttributeData); ok {
		return fmt.Sprintf("%d/%d/%d", data.CreationTime.HighDateTime, data.CreationTime.LowDateTime, data.FileAttributes)
	}
	return ""
}
