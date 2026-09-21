//go:build !darwin && !linux

package hostfs

import (
	"io/fs"
	"os"

	hosts "github.com/veypi/aic-pod/protocol/fs"
)

func atomicReplaceSupported() bool { return false }
func safeReadSupported() bool      { return false }
func openRegular(root *os.Root, name string) (*os.File, error) {
	return nil, hosts.Fail("unsupported", "Safe regular-file opening is unavailable on this platform")
}
func fileIdentity(info fs.FileInfo) string { return "" }
