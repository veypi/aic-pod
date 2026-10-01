package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/veypi/aic-pod/skill-packages/browser/provider/chrome"
)

// stagedDownload 登记一条下载记录；bytes > 0 时同时落盘对应文件。
func stagedDownload(t *testing.T, s *Service, id, pageID, state string, bytes int64, created time.Time) *download {
	t.Helper()
	path := filepath.Join(s.cfg.StateDir, "downloads", id)
	if bytes > 0 {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, make([]byte, bytes), 0600); err != nil {
			t.Fatal(err)
		}
	}
	d := &download{ID: id, PageID: pageID, State: state, Bytes: bytes, guid: "guid_" + id, path: path, created: created}
	s.downloads[id] = d
	return d
}

func TestDownloadsReclaimedWithPageClose(t *testing.T) {
	s := New(Config{StateDir: t.TempDir()})
	defer s.Close()
	p := &page{info: PageInfo{ID: "p_test"}}
	s.pages[p.info.ID] = p
	now := time.Now()
	done := stagedDownload(t, s, "d_done", "p_test", "completed", 4, now)
	live := stagedDownload(t, s, "d_live", "p_test", "inProgress", 4, now)
	other := stagedDownload(t, s, "d_other", "p_other", "completed", 4, now)
	s.remove(p)
	if len(s.downloads) != 1 || s.downloads["d_other"] == nil {
		t.Fatal("page close reclaimed foreign downloads:", len(s.downloads))
	}
	for _, d := range []*download{done, live} {
		if _, err := os.Stat(d.path); !os.IsNotExist(err) {
			t.Fatalf("page close retained %s file: %v", d.State, err)
		}
	}
	if _, err := os.Stat(other.path); err != nil {
		t.Fatal("page close removed foreign download:", err)
	}
	// inProgress 的 CDP cancel 需真实连接（conn == nil 时跳过），由 live 测试覆盖。
}

func TestEvictDownloadsDropsOldestCompleted(t *testing.T) {
	s := New(Config{StateDir: t.TempDir(), MaxDownloadBytes: 10, MaxTotalDownloadBytes: 25})
	defer s.Close()
	now := time.Now()
	old := stagedDownload(t, s, "d_old", "p_test", "completed", 10, now.Add(-3*time.Second))
	stagedDownload(t, s, "d_mid", "p_test", "completed", 10, now.Add(-2*time.Second))
	stagedDownload(t, s, "d_new", "p_test", "completed", 5, now.Add(-time.Second))
	s.evictDownloads()
	// 总量 25 + 预留 10 > 25 → 淘汰最旧一份；15 + 10 <= 25 → 停。
	if s.downloads["d_old"] != nil {
		t.Fatal("oldest completed download retained under capacity pressure")
	}
	if _, err := os.Stat(old.path); !os.IsNotExist(err) {
		t.Fatal("evicted download file retained:", err)
	}
	if s.downloads["d_mid"] == nil || s.downloads["d_new"] == nil {
		t.Fatal("newer completed downloads evicted")
	}
	if _, err := os.Stat(s.downloads["d_mid"].path); err != nil {
		t.Fatal("eviction removed retained file:", err)
	}
}

func TestEvictDownloadsNeverDropsInProgress(t *testing.T) {
	s := New(Config{StateDir: t.TempDir(), MaxDownloadBytes: 10, MaxTotalDownloadBytes: 15})
	defer s.Close()
	now := time.Now()
	live := stagedDownload(t, s, "d_live", "p_test", "inProgress", 10, now.Add(-time.Hour))
	stagedDownload(t, s, "d_done", "p_test", "completed", 10, now)
	s.evictDownloads()
	// 淘汰 d_done 后总量 10 + 预留 10 仍 > 15，但 inProgress 豁免。
	if s.downloads["d_live"] != live {
		t.Fatal("in-progress download passively evicted")
	}
	if _, err := os.Stat(live.path); err != nil {
		t.Fatal("in-progress download file removed:", err)
	}
	if s.downloads["d_done"] != nil {
		t.Fatal("completed download retained over capacity")
	}
}

func TestEvictDownloadsRespectsCountCap(t *testing.T) {
	s := New(Config{StateDir: t.TempDir()})
	defer s.Close()
	now := time.Now()
	for i := 0; i < maxDownloads; i++ {
		id := fmt.Sprintf("d_%03d", i)
		s.downloads[id] = &download{ID: id, PageID: "p_test", State: "completed", created: now.Add(time.Duration(i) * time.Second), path: filepath.Join(s.cfg.StateDir, "downloads", id)}
	}
	s.evictDownloads()
	if len(s.downloads) != maxDownloads-1 {
		t.Fatal("count cap eviction wrong:", len(s.downloads))
	}
	if s.downloads["d_000"] != nil {
		t.Fatal("oldest record retained at count cap")
	}
}

func TestDownloadsHaveNoClockExpiry(t *testing.T) {
	s := New(Config{StateDir: t.TempDir(), MaxDownloadBytes: 10, MaxTotalDownloadBytes: 100})
	defer s.Close()
	old := stagedDownload(t, s, "d_old", "p_test", "completed", 4, time.Now().Add(-24*time.Hour))
	s.evictDownloads()
	if s.downloads["d_old"] != old {
		t.Fatal("old download reclaimed without capacity pressure")
	}
	got, err := s.DownloadGet(context.Background(), DownloadArgs{ID: "d_old"})
	if err != nil || got.ID != "d_old" {
		t.Fatal("old download expired by clock:", err)
	}
}

func TestDownloadWillBeginEvictsToAdmit(t *testing.T) {
	s := New(Config{StateDir: t.TempDir(), MaxDownloadBytes: 10, MaxTotalDownloadBytes: 15})
	defer s.Close()
	p := &page{info: PageInfo{ID: "p_test"}}
	s.frames["frame1"] = p
	stagedDownload(t, s, "d_done", "p_test", "completed", 10, time.Now())
	params, _ := json.Marshal(map[string]any{"guid": "guid_new", "frameId": "frame1", "suggestedFilename": "a.bin", "url": "https://example.com/a.bin"})
	s.downloadEvent(nil, chrome.Event{Method: "Browser.downloadWillBegin", Params: params})
	if s.downloads["d_done"] != nil {
		t.Fatal("registration did not evict for capacity")
	}
	found := false
	for _, d := range s.downloads {
		if d.guid == "guid_new" && d.State == "inProgress" {
			found = true
		}
	}
	if !found {
		t.Fatal("new download not registered")
	}
}
