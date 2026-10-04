//go:build unix

package host

import "golang.org/x/sys/unix"

func secureOpenFlags(flags int) int { return flags | unix.O_NOFOLLOW | unix.O_NONBLOCK }
