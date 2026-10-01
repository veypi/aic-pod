package host

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/veypi/aic-pod/libs/hostfs"
	fsp "github.com/veypi/aic-pod/protocol/fs"
)

// Mount filesystem roots, not WorkDir. WorkDir is only the initial directory.
// Internal mount IDs never replace full native paths in public /fs URLs.
//
// 返回 home（fs home = WorkDir，工作区语义）与 osHome（OS 用户主目录，
// ~/.aic 等运行数据定位用——两者在 pod 上是不同目录，不可混用）。osHome
// 未落在任一挂载根内时返回零值（fs os_home 方法报不可用）。
func deviceFileRoots(workdir string) ([]hostfs.Root, fsp.Path, fsp.Path, error) {
	roots := make([]hostfs.Root, 0)
	var home, osHome fsp.Path
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
		if dir, err := os.UserHomeDir(); err == nil {
			if rel, err := filepath.Rel(path, dir); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				osHome = fsp.Path{RootID: id, Segments: []string{}}
				if rel != "." {
					osHome.Segments = strings.Split(filepath.ToSlash(rel), "/")
				}
			}
		}
	}
	if home.RootID == "" {
		return nil, home, osHome, fmt.Errorf("workspace has no filesystem volume")
	}
	return roots, home, osHome, nil
}

// osHomePtr 零值 osHome（未落在任一挂载根内）→ nil（fs os_home 报不可用）。
func osHomePtr(p fsp.Path) *fsp.Path {
	if p.RootID == "" {
		return nil
	}
	return &p
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
