package browser

import (
	"context"
	"encoding/json"
	"github.com/veypi/aic-pod/libs/browser/chrome"
	tool "github.com/veypi/aic-pod/libs/hosts_tool"
	wire "github.com/veypi/aic-pod/protocol/hosts_tools"
	"io"
	"os"
	"path/filepath"
	"time"
)

type download struct {
	ID                  string `json:"download_id"`
	PageID              string `json:"page_id"`
	Name                string `json:"name"`
	URL                 string `json:"url"`
	State               string `json:"state"`
	Bytes               int64  `json:"bytes"`
	guid, subject, path string
	created             time.Time
}

func (s *Service) downloadEvent(conn *chrome.Conn, e chrome.Event) {
	var v struct {
		GUID     string  `json:"guid"`
		Frame    string  `json:"frameId"`
		Name     string  `json:"suggestedFilename"`
		URL      string  `json:"url"`
		State    string  `json:"state"`
		Received float64 `json:"receivedBytes"`
		Total    float64 `json:"totalBytes"`
	}
	if json.Unmarshal(e.Params, &v) != nil || !wire.ValidID(v.GUID) {
		return
	}
	cancelDownload := func() {
		go func() {
			ctx, cancel := context.WithTimeout(s.ctx, 3*time.Second)
			defer cancel()
			_ = conn.Call(ctx, "", "Browser.cancelDownload", map[string]any{"guid": v.GUID}, nil)
		}()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if e.Method == "Browser.downloadWillBegin" {
		p := s.frames[v.Frame]
		if p == nil || len(s.downloads) >= 128 {
			cancelDownload()
			return
		}
		d := &download{ID: wire.NewID("d_"), PageID: p.info.ID, Name: bounded(filepath.Base(v.Name), 512), URL: bounded(v.URL, 8192), State: "inProgress", guid: v.GUID, subject: p.subject, path: filepath.Join(s.cfg.StateDir, "downloads", v.GUID), created: time.Now()}
		s.downloads[d.ID] = d
		return
	}
	var d *download
	var total int64
	for id, item := range s.downloads {
		if time.Since(item.created) > s.cfg.DownloadTTL && item.State != "inProgress" {
			_ = os.Remove(item.path)
			delete(s.downloads, id)
			continue
		}
		if item.guid == v.GUID {
			d = item
		} else {
			total += item.Bytes
		}
	}
	if d == nil {
		cancelDownload()
		return
	}
	d.Bytes = int64(v.Received)
	if d.Bytes > s.cfg.MaxDownloadBytes || int64(v.Total) > s.cfg.MaxDownloadBytes || total+d.Bytes > s.cfg.MaxTotalDownloadBytes {
		d.State = "cancelled"
		cancelDownload()
		_ = os.Remove(d.path)
		_ = os.Remove(d.path + ".crdownload")
		return
	}
	d.State = v.State
	if v.State == "completed" {
		st, err := os.Lstat(d.path)
		if err != nil || !st.Mode().IsRegular() || st.Size() > s.cfg.MaxDownloadBytes || total+st.Size() > s.cfg.MaxTotalDownloadBytes {
			d.State = "failed"
			_ = os.Remove(d.path)
		} else {
			d.Bytes = st.Size()
		}
	}
	if v.State == "canceled" {
		_ = os.Remove(d.path)
		_ = os.Remove(d.path + ".crdownload")
	}
}
func (s *Service) DownloadList(ctx context.Context, c tool.Caller, a PageArgs) ([]download, error) {
	if _, err := s.get(c, a.PageID); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []download{}
	for _, d := range s.downloads {
		if d.subject == c.Subject && d.PageID == a.PageID {
			out = append(out, *d)
		}
	}
	return out, nil
}
func (s *Service) DownloadGet(ctx context.Context, c tool.Caller, a DownloadArgs) (download, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := s.downloads[a.ID]
	if d == nil || d.subject != c.Subject || time.Since(d.created) > s.cfg.DownloadTTL {
		return download{}, wire.Fail("not_found", "Download expired or unavailable")
	}
	return *d, nil
}
func (s *Service) DownloadWait(ctx context.Context, c tool.Caller, a DownloadArgs) (download, error) {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		d, err := s.DownloadGet(ctx, c, a)
		if err != nil {
			return d, err
		}
		if d.State != "inProgress" {
			return d, nil
		}
		select {
		case <-ctx.Done():
			return download{}, ctx.Err()
		case <-ticker.C:
		}
	}
}
func (s *Service) DownloadCancel(ctx context.Context, c tool.Caller, a DownloadArgs) (map[string]bool, error) {
	d, err := s.DownloadGet(ctx, c, a)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	conn := s.conn
	s.mu.Unlock()
	if conn == nil {
		return nil, wire.Fail("unavailable", "Chrome is unavailable")
	}
	err = conn.Call(ctx, "", "Browser.cancelDownload", map[string]any{"guid": d.guid}, nil)
	return map[string]bool{"cancel_requested": err == nil}, err
}
func (s *Service) checkFile(ctx context.Context, c tool.Caller, path string, write bool) (string, error) {
	if s.cfg.CheckFile == nil {
		return "", wire.Fail("permission_denied", "File access is not configured")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	if err = s.cfg.CheckFile(ctx, c, abs, write); err != nil {
		return "", err
	}
	return abs, nil
}
func (s *Service) DownloadExport(ctx context.Context, c tool.Caller, a DownloadArgs) (map[string]any, error) {
	d, err := s.DownloadGet(ctx, c, a)
	if err != nil {
		return nil, err
	}
	if d.State != "completed" {
		return nil, wire.Fail("not_ready", "Download not complete")
	}
	if a.Path == "" {
		return nil, errArg("path is required")
	}
	dest, err := s.checkFile(ctx, c, a.Path, true)
	if err != nil {
		return nil, err
	}
	input, err := os.Open(d.path)
	if err != nil {
		return nil, err
	}
	defer input.Close()
	out, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return nil, err
	}
	success := false
	defer func() {
		out.Close()
		if !success {
			_ = os.Remove(dest)
		}
	}()
	written, err := copyChecked(ctx, out, input, func() error { _, e := s.checkFile(ctx, c, dest, true); return e })
	if err != nil {
		return nil, err
	}
	if err = out.Close(); err != nil {
		return nil, err
	}
	success = true
	return map[string]any{"path": dest, "bytes": written}, nil
}
func copyChecked(ctx context.Context, out io.Writer, in io.Reader, check func() error) (int64, error) {
	buf := make([]byte, 32<<10)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		if check != nil {
			if err := check(); err != nil {
				return total, err
			}
		}
		n, err := in.Read(buf)
		if n > 0 {
			w, e := out.Write(buf[:n])
			total += int64(w)
			if e != nil {
				return total, e
			}
			if w != n {
				return total, io.ErrShortWrite
			}
		}
		if err == io.EOF {
			return total, nil
		}
		if err != nil {
			return total, err
		}
	}
}
func (s *Service) Upload(ctx context.Context, c tool.Caller, a UploadArgs) (Result, error) {
	if err := a.Locator.Validate(); err != nil {
		return Result{}, err
	}
	p, err := s.get(c, a.PageID)
	if err != nil {
		return Result{}, err
	}
	source, err := s.checkFile(ctx, c, a.File, false)
	if err != nil {
		return Result{}, err
	}
	file, err := os.Open(source)
	if err != nil {
		return Result{}, err
	}
	defer file.Close()
	st, err := file.Stat()
	if err != nil || !st.Mode().IsRegular() || st.Size() > s.cfg.MaxDownloadBytes {
		return Result{}, errArg("Upload must be a bounded regular file")
	}
	dir := filepath.Join(s.cfg.StateDir, "uploads")
	if err = os.MkdirAll(dir, 0700); err != nil {
		return Result{}, err
	}
	private, err := os.MkdirTemp(dir, "upload-*")
	if err != nil {
		return Result{}, err
	}
	defer os.RemoveAll(private)
	stage, err := os.OpenFile(filepath.Join(private, filepath.Base(source)), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return Result{}, err
	}
	defer os.Remove(stage.Name())
	defer stage.Close()
	copied, err := copyChecked(ctx, stage, io.LimitReader(file, s.cfg.MaxDownloadBytes+1), func() error { _, e := s.checkFile(ctx, c, source, false); return e })
	if err != nil {
		return Result{}, err
	}
	if copied > s.cfg.MaxDownloadBytes {
		return Result{}, errArg("Upload exceeds limit")
	}
	if err = stage.Close(); err != nil {
		return Result{}, err
	}
	err = p.write(ctx, c, func() error {
		object, err := p.resolve(ctx, c, a.Locator)
		if err != nil {
			return err
		}
		defer p.release(object)
		return p.call(ctx, "DOM.setFileInputFiles", map[string]any{"files": []string{stage.Name()}, "objectId": object}, nil)
	})
	return Result{PageInfo: p.snapshot(), Effect: effect(err)}, err
}

type DownloadReadArgs struct {
	DownloadArgs
	Offset int64 `json:"offset,omitempty"`
	Limit  int   `json:"limit,omitempty"`
}
type DownloadPart struct {
	Bytes  []byte `json:"bytes"`
	Offset int64  `json:"offset"`
	Total  int64  `json:"total"`
	EOF    bool   `json:"eof"`
}

func (s *Service) DownloadRead(ctx context.Context, c tool.Caller, a DownloadReadArgs) (DownloadPart, error) {
	d, err := s.DownloadGet(ctx, c, a.DownloadArgs)
	if err != nil {
		return DownloadPart{}, err
	}
	if d.State != "completed" {
		return DownloadPart{}, wire.Fail("not_ready", "Download not complete")
	}
	if a.Offset < 0 || a.Offset > d.Bytes || a.Limit < 0 || a.Limit > 32<<10 {
		return DownloadPart{}, errArg("Invalid download range")
	}
	if a.Limit == 0 {
		a.Limit = 32 << 10
	}
	if err := c.Validate(ctx); err != nil {
		return DownloadPart{}, err
	}
	f, err := os.Open(d.path)
	if err != nil {
		return DownloadPart{}, err
	}
	defer f.Close()
	buf := make([]byte, min(int64(a.Limit), d.Bytes-a.Offset))
	n, err := f.ReadAt(buf, a.Offset)
	if err != nil && err != io.EOF {
		return DownloadPart{}, err
	}
	return DownloadPart{Bytes: buf[:n], Offset: a.Offset, Total: d.Bytes, EOF: a.Offset+int64(n) >= d.Bytes}, nil
}

// Called with s.mu held. Completed stages expire without requiring another download.
func (s *Service) pruneDownloads() {
	for id, d := range s.downloads {
		if time.Since(d.created) <= s.cfg.DownloadTTL {
			continue
		}
		if d.State == "inProgress" {
			conn, guid := s.conn, d.guid
			if conn != nil {
				go func() {
					ctx, cancel := context.WithTimeout(s.ctx, 3*time.Second)
					defer cancel()
					_ = conn.Call(ctx, "", "Browser.cancelDownload", map[string]any{"guid": guid}, nil)
				}()
			}
		}
		_ = os.Remove(d.path)
		_ = os.Remove(d.path + ".crdownload")
		delete(s.downloads, id)
	}
}
