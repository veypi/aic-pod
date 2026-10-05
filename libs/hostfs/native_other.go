//go:build !darwin && !linux && !windows

package hostfs

import (
	"github.com/veypi/aic-pod/protocol"
	"io/fs"
	"os"
)

func atomicReplaceSupported() bool { return false }
func safeReadSupported() bool      { return false }
func openRegular(root *os.Root, name string) (*os.File, error) {
	return nil, protocol.FSFail("unsupported", "Safe regular-file opening is unavailable on this platform")
}
func fileIdentity(info fs.FileInfo) string { return "" }

func entryHidden(fs.DirEntry) bool { return false }
func dirLink(fs.FileInfo) bool     { return false }
