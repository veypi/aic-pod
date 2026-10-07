//go:build !linux && !darwin && !windows

package hostfs

import "github.com/veypi/aic-pod/protocol"

func renameNoReplace(from int, src string, to int, dst string) error {
	return protocol.FSFail("unsupported", "Atomic no-replace move is unavailable")
}

func renameReplace(from int, src string, to int, dst string) error {
	return protocol.FSFail("unsupported", "Atomic move is unavailable")
}
