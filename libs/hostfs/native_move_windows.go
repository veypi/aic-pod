//go:build windows

package hostfs

import (
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

func renameNoReplace(from int, src string, to int, dst string) error {
	return renameWindows(from, src, to, dst, false)
}

func renameReplace(from int, src string, to int, dst string) error {
	return renameWindows(from, src, to, dst, true)
}

// Rename through pinned directory handles. In particular, absent means a
// single atomic no-replace operation, not an existence check followed by move.
// https://learn.microsoft.com/en-us/windows-hardware/drivers/ddi/ntifs/ns-ntifs-_file_rename_information
func renameWindows(from int, src string, to int, dst string, replace bool) error {
	if dst == "" || dst == "." || dst == ".." || strings.ContainsAny(dst, `/\:`) {
		return syscall.EINVAL
	}
	handle, err := openRelative(windows.Handle(from), src, windows.DELETE, windows.FILE_OPEN_FOR_BACKUP_INTENT)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(handle)
	name, err := windows.UTF16FromString(dst)
	if err != nil {
		return err
	}
	// Both FILE_RENAME_INFORMATION and its Ex variant have the same layout.
	// Only a single filename is accepted, so MAX_PATH bounds the buffer.
	var info struct {
		Flags          uint32
		RootDirectory  windows.Handle
		FileNameLength uint32
		FileName       [windows.MAX_PATH]uint16
	}
	if len(name) > len(info.FileName) {
		return syscall.ENAMETOOLONG
	}
	info.RootDirectory = windows.Handle(to)
	info.FileNameLength = uint32((len(name) - 1) * 2)
	copy(info.FileName[:], name)
	if replace {
		info.Flags = windows.FILE_RENAME_REPLACE_IF_EXISTS | windows.FILE_RENAME_POSIX_SEMANTICS
		err = windows.NtSetInformationFile(handle, &windows.IO_STATUS_BLOCK{}, (*byte)(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)), 65 /* FileRenameInformationEx */)
		if err == nil {
			return nil
		}
		// Older Windows/filesystems may not support Ex/POSIX rename. The basic
		// operation still replaces atomically; it can reject open destinations.
		info.Flags = 1
	}
	return windowsFileError(windows.NtSetInformationFile(handle, &windows.IO_STATUS_BLOCK{}, (*byte)(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)), windows.FileRenameInformation))
}
