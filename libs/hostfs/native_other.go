//go:build !darwin && !linux

package hostfs

import (
	"io/fs"
	"os"

	"github.com/veypi/aic-pod/protocol/hosts"
)

func atomicReplaceSupported() bool { return false }
func safeReadSupported() bool      { return false }
func openRegular(root *os.Root, name string) (*os.File, error) {
	return nil, hosts.Fail("unsupported", "Safe regular-file opening is unavailable on this platform")
}
func fileIdentity(info fs.FileInfo) string { return "" }
