package host

import (
	"fmt"
	"net/url"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/veypi/aic-pod/libs/hostfs"
	fsp "github.com/veypi/aic-pod/protocol/fs"
)

// Mount filesystem roots, not WorkDir. WorkDir is only the initial directory.
// Internal mount IDs never replace full native paths in public /fs URLs.
func deviceFileRoots(workdir string) ([]hostfs.Root, fsp.Path, error) {
	roots := make([]hostfs.Root, 0)
	var home fsp.Path
	for _, path := range filesystemRoots() {
		if runtime.GOOS == "windows" && path == "/" {
			continue
		}
		volume := filepath.VolumeName(path)
		id := "filesystem"
		if volume != "" {
			id = "drive_" + strings.TrimSuffix(strings.ToUpper(volume), ":")
			path = volume + string(filepath.Separator)
		}
		rel, err := filepath.Rel(path, workdir)
		contains := err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
		roots = append(roots, hostfs.Root{ID: id, Name: filepath.ToSlash(path), Path: path, Default: contains})
		if contains {
			home = fsp.Path{RootID: id, Segments: []string{}}
			if rel != "." {
				home.Segments = strings.Split(filepath.ToSlash(rel), "/")
			}
		}
	}
	if home.RootID == "" {
		return nil, home, fmt.Errorf("workspace has no filesystem volume")
	}
	return roots, home, nil
}

// Public file URLs retain full device paths, including files outside WorkDir.
func (c *Client) attachFileURL(attrs map[string]string) {
	if attrs == nil || attrs["path"] == "" || !filepath.IsAbs(attrs["path"]) {
		return
	}
	p := filepath.ToSlash(attrs["path"])
	parts := strings.Split(strings.TrimPrefix(p, "/"), "/")
	for i, part := range parts {
		parts[i] = url.PathEscape(part)
	}
	attrs["file_url"] = "/fs/" + c.hostID + "/" + strings.Join(parts, "/")
}
