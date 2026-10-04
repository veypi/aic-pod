package host

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/veypi/aic-pod/libs/hostfs"
	"github.com/veypi/vbox"
)

// pinOSParent walks the canonical parent using directory handles. A link or
// replaced directory encountered after resolution fails instead of redirecting
// a subsequent open/rename. This mirrors the structured hostfs provider.
func pinOSParent(name string, follow bool) (*os.Root, string, string, error) {
	canonical := vbox.CanonicalNoFollow(name)
	if follow {
		canonical = vbox.Canonical(name)
	}
	abs, err := toOS(canonical)
	if err != nil {
		return nil, "", "", err
	}
	if !filepath.IsAbs(abs) {
		return nil, "", "", fmt.Errorf("absolute host path required")
	}
	volume := filepath.VolumeName(abs) + string(filepath.Separator)
	h, err := os.OpenRoot(volume)
	if err != nil {
		return nil, "", "", err
	}
	rel, err := filepath.Rel(volume, filepath.Dir(abs))
	if err != nil {
		h.Close()
		return nil, "", "", err
	}
	if rel != "." {
		for _, part := range strings.Split(rel, string(filepath.Separator)) {
			info, err := h.Lstat(part)
			if err != nil || !info.IsDir() || info.Mode()&fs.ModeSymlink != 0 {
				h.Close()
				return nil, "", "", fmt.Errorf("directory changed during lookup: %s", abs)
			}
			next, err := h.OpenRoot(part)
			if err != nil {
				h.Close()
				return nil, "", "", err
			}
			actual, err := next.Stat(".")
			h.Close()
			if err != nil || !os.SameFile(info, actual) {
				next.Close()
				return nil, "", "", fmt.Errorf("directory changed during lookup: %s", abs)
			}
			h = next
		}
	}
	base := filepath.Base(abs)
	if abs == volume {
		base = "."
	}
	return h, base, canonical, nil
}

func (OSVFS) Lstat(name string) (fs.FileInfo, error) {
	if isVirtualRoot(name) {
		return virtualDir("/"), nil
	}
	p, err := toOS(name)
	if err != nil {
		return nil, err
	}
	return os.Lstat(p)
}

func (OSVFS) OpenChecked(name string, flags int, perm fs.FileMode, check func(string, vbox.FileOp) error) (*os.File, error) {
	h, base, canonical, err := pinOSParent(name, flags&(os.O_CREATE|os.O_EXCL) != os.O_CREATE|os.O_EXCL)
	if err != nil {
		return nil, err
	}
	defer h.Close()
	write := flags&(os.O_WRONLY|os.O_RDWR|os.O_CREATE|os.O_TRUNC|os.O_APPEND) != 0
	if flags&(os.O_WRONLY|os.O_RDWR) != os.O_WRONLY {
		if err := check(canonical, vbox.OpRead); err != nil {
			return nil, err
		}
	}
	if write {
		if err := check(canonical, vbox.OpWrite); err != nil {
			return nil, err
		}
	}
	info, err := h.Lstat(base)
	if errors.Is(err, fs.ErrNotExist) && flags&os.O_CREATE != 0 {
		return h.OpenFile(base, flags|os.O_EXCL, perm)
	}
	if err != nil {
		return nil, err
	}
	if flags&(os.O_CREATE|os.O_EXCL) == os.O_CREATE|os.O_EXCL {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrExist}
	}
	// Preserve ordinary shell device I/O (e.g. /dev/null). Transfer commands
	// independently require regular files/directories before opening anything.
	device := info.Mode()&fs.ModeDevice != 0
	if !info.Mode().IsRegular() && !(info.IsDir() && !write) && !device {
		return nil, fmt.Errorf("open: special file or symlink is unsupported: %s", name)
	}
	// Never truncate before comparing the opened object's identity.
	f, err := h.OpenFile(base, secureOpenFlags(flags&^(os.O_TRUNC|os.O_CREATE)), perm)
	if err != nil {
		return nil, err
	}
	actual, err := f.Stat()
	if err != nil || !os.SameFile(info, actual) {
		f.Close()
		return nil, fmt.Errorf("file changed during open: %s", name)
	}
	if flags&os.O_TRUNC != 0 && actual.Mode().IsRegular() {
		if err := f.Truncate(0); err != nil {
			f.Close()
			return nil, err
		}
	}
	return f, nil
}

func (v OSVFS) ReadDirChecked(name string, check func(string, vbox.FileOp) error) ([]fs.DirEntry, error) {
	if isVirtualRoot(name) {
		return v.ReadDir(name)
	}
	f, err := v.OpenChecked(name, os.O_RDONLY, 0, check)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return f.ReadDir(-1)
}

func (v OSVFS) MkdirAllChecked(name string, perm fs.FileMode, check func(string, vbox.FileOp) error) error {
	canonical := vbox.Canonical(name)
	abs, err := toOS(canonical)
	if err != nil {
		return err
	}
	if info, err := os.Stat(abs); err == nil {
		if info.IsDir() {
			return nil
		}
		return fmt.Errorf("not a directory: %s", name)
	}
	parent := filepath.Dir(abs)
	if parent == abs {
		return fmt.Errorf("cannot create filesystem root")
	}
	if err := v.MkdirAllChecked(hostCanonical(parent), perm, check); err != nil {
		return err
	}
	h, base, canonical, err := pinOSParent(canonical, false)
	if err != nil {
		return err
	}
	defer h.Close()
	if err := check(canonical, vbox.OpWrite); err != nil {
		return err
	}
	if err := h.Mkdir(base, perm); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	info, err := h.Lstat(base)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&fs.ModeSymlink != 0 {
		return fmt.Errorf("directory changed during mkdir")
	}
	return nil
}

func (OSVFS) RenameChecked(from, to string, check func(string, vbox.FileOp) error) error {
	src, a, ca, err := pinOSParent(from, false)
	if err != nil {
		return err
	}
	defer src.Close()
	dst, b, cb, err := pinOSParent(to, false)
	if err != nil {
		return err
	}
	defer dst.Close()
	if err := check(ca, vbox.OpWrite); err != nil {
		return err
	}
	if err := check(cb, vbox.OpWrite); err != nil {
		return err
	}
	return hostfs.RenamePinned(src, a, dst, b)
}

func (OSVFS) RemoveChecked(name string, check func(string, vbox.FileOp) error) error {
	h, base, canonical, err := pinOSParent(name, false)
	if err != nil {
		return err
	}
	defer h.Close()
	if err := check(canonical, vbox.OpWrite); err != nil {
		return err
	}
	return h.Remove(base)
}
