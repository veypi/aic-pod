package browser

import (
	"os"
	"testing"

	wire "github.com/veypi/aic-pod/protocol/hosts_tools"
)

func TestUploadsReserveCapacityAndReleaseWithPage(t *testing.T) {
	s := New(Config{StateDir: t.TempDir(), MaxUploads: 2, MaxTotalUploadBytes: 10})
	defer s.Close()
	p := &page{info: PageInfo{ID: "p_test"}}
	s.pages[p.info.ID] = p
	dir, err := s.reserveUpload(p, 6)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.reserveUpload(p, 5); err == nil || wire.AsFault(err).Code != "overloaded" {
		t.Fatal("byte quota ignored", err)
	}
	if _, err = s.reserveUpload(p, 0); err != nil {
		t.Fatal(err)
	}
	if _, err = s.reserveUpload(p, 0); err == nil {
		t.Fatal("count quota ignored")
	}
	s.remove(p)
	if _, err = os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("page removal retained upload", err)
	}
	if len(s.uploads) != 0 {
		t.Fatal("upload reservations leaked")
	}
	if _, err = s.reserveUpload(p, 1); err == nil {
		t.Fatal("closed page admitted upload")
	}
	s.pages[p.info.ID] = p
	dir, err = s.reserveUpload(p, 10)
	if err != nil {
		t.Fatal("released capacity unavailable", err)
	}
	s.releaseUpload(dir)
	if len(s.uploads) != 0 {
		t.Fatal("failed upload retained reservation")
	}
}
