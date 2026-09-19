package host

import (
	"net/url"
	"path/filepath"
	"strings"
	"testing"
)

func TestAIFileReferencesKeepFullNativePaths(t *testing.T) {
	c := New(Options{WorkDir: t.TempDir()})
	c.hostID = "device"
	outside := t.TempDir()
	for _, p := range []string{filepath.Join(c.opts.WorkDir, "a b%.txt"), filepath.Join(outside, "中文%2F?#.txt"), filepath.VolumeName(outside) + string(filepath.Separator)} {
		attrs := map[string]string{"path": p}
		c.attachFileURL(attrs)
		u, err := url.Parse(attrs["file_url"])
		if err != nil || u.RawQuery != "" || u.Fragment != "" {
			t.Fatalf("invalid link: %+v %v", attrs, err)
		}
		want := "/fs/device/" + strings.TrimPrefix(filepath.ToSlash(p), "/")
		if u.Path != want {
			t.Fatalf("full path changed: %q != %q", u.Path, want)
		}
		if strings.Contains(attrs["file_url"], "root_") {
			t.Fatal("workspace root leaked into public URL")
		}
	}
	attrs := map[string]string{"path": "relative.txt"}
	c.attachFileURL(attrs)
	if attrs["file_url"] != "" {
		t.Fatal("invented absolute link for a relative path")
	}
}
