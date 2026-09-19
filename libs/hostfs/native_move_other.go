//go:build !linux && !darwin

package hostfs

import "github.com/veypi/aic-pod/protocol/hosts"

func renameNoReplace(from int, src string, to int, dst string) error {
	return hosts.Fail("unsupported", "Atomic no-replace move is unavailable")
}

func renameReplace(from int, src string, to int, dst string) error {
	return hosts.Fail("unsupported", "Atomic move is unavailable")
}
