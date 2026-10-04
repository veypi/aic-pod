//go:build !unix

package host

// os.Root confines symlink resolution; OpenChecked verifies identity before
// reading or truncating. Windows has no O_NOFOLLOW flag in os.OpenFile.
func secureOpenFlags(flags int) int { return flags }
