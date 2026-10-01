package browser

import (
	"context"
	"encoding/json"
	wire "github.com/veypi/aic-pod/protocol/hosts_tools"
	"github.com/veypi/aic-pod/skill-packages/browser/provider/chrome"
	"io"
	"os"
	"path/filepath"
	"time"
)

type download struct {
	ID         string `json:"download_id"`
	PageID     string `json:"page_id"`
	Name       string `json:"name"`
	URL        string `json:"url"`
	State      string `json:"state"`
	Bytes      int64  `json:"bytes"`
	guid, path string
	created    time.Time // 只用于容量淘汰的先后排序，无墙钟过期
}

// maxDownloads 是下载记录数上限，只防无限堆积；满容时先淘汰最旧的非进行中
// 记录，全为进行中才拒绝新下载。
const maxDownloads = 128

// Chrome's File objects may read from disk long after setFileInputFiles returns,
// including after a form submission starts. Retain uploads until page closure;
// enforce count/byte limits instead of expiring live File objects by a timer.
type upload struct {
	page  *page
	bytes int64
}

func (s *Service) reserveUpload(p *page, size int64) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.pages[p.info.ID] != p {
		return "", wire.Fail("closed", "Upload page closed")
	}
	total := size
	for _, u := range s.uploads {
		total += u.bytes
	}
	if len(s.uploads) >= s.cfg.MaxUploads || total > s.cfg.MaxTotalUploadBytes {
		return "", wire.Fail("overloaded", "Upload storage limit reached; close pages holding uploaded files")
	}
	dir := filepath.Join(s.cfg.StateDir, "uploads")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	private, err := os.MkdirTemp(dir, "upload-*")
	if err == nil {
		s.uploads[private] = upload{page: p, bytes: size}
	}
	return private, err
}

func (s *Service) releaseUpload(dir string) {
	s.mu.Lock()
	delete(s.uploads, dir)
	s.mu.Unlock()
	_ = os.RemoveAll(dir)
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
		if p == nil {
			cancelDownload()
			return
		}
		s.evictDownloads()
		if len(s.downloads) >= maxDownloads {
			cancelDownload()
			return
		}
		d := &download{ID: wire.NewID("d_"), PageID: p.info.ID, Name: bounded(filepath.Base(v.Name), 512), URL: bounded(v.URL, 8192), State: "inProgress", guid: v.GUID, path: filepath.Join(s.cfg.StateDir, "downloads", v.GUID), created: time.Now()}
		s.downloads[d.ID] = d
		return
	}
	var d *download
	var total int64
	for _, item := range s.downloads {
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
func (s *Service) DownloadList(ctx context.Context, a PageArgs) ([]download, error) {
	if _, err := s.get(a.PageID); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []download{}
	for _, d := range s.downloads {
		if d.PageID == a.PageID {
			out = append(out, *d)
		}
	}
	return out, nil
}
func (s *Service) DownloadGet(ctx context.Context, a DownloadArgs) (download, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := s.downloads[a.ID]
	if d == nil {
		return download{}, wire.Fail("not_found", "Download unavailable")
	}
	return *d, nil
}
func (s *Service) DownloadWait(ctx context.Context, a DownloadArgs) (download, error) {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		d, err := s.DownloadGet(ctx, a)
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
func (s *Service) DownloadCancel(ctx context.Context, a DownloadArgs) (map[string]bool, error) {
	d, err := s.DownloadGet(ctx, a)
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
func (s *Service) DownloadExport(ctx context.Context, a DownloadArgs) (map[string]any, error) {
	d, err := s.DownloadGet(ctx, a)
	if err != nil {
		return nil, err
	}
	if d.State != "completed" {
		return nil, wire.Fail("not_ready", "Download not complete")
	}
	if a.Path == "" {
		return nil, errArg("path is required")
	}
	// 文件权限 = 进程沙箱（v6 P5：v5 的文件门回调已删）；相对路径由 svc 在
	// 调用边界按 invoke cwd 绝对化，此处只兜底。
	dest, err := filepath.Abs(a.Path)
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
	written, err := copyChecked(ctx, out, input)
	if err != nil {
		return nil, err
	}
	if err = out.Close(); err != nil {
		return nil, err
	}
	success = true
	return map[string]any{"path": dest, "bytes": written}, nil
}
func copyChecked(ctx context.Context, out io.Writer, in io.Reader) (int64, error) {
	buf := make([]byte, 32<<10)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
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
func (s *Service) Upload(ctx context.Context, a UploadArgs) (Result, error) {
	if err := a.Locator.Validate(); err != nil {
		return Result{}, err
	}
	p, err := s.get(a.PageID)
	if err != nil {
		return Result{}, err
	}
	// 相对路径由 svc 在调用边界按 invoke cwd 绝对化（见 DownloadExport）。
	source, err := filepath.Abs(a.File)
	if err != nil {
		return Result{}, err
	}
	file, err := os.Open(source)
	if err != nil {
		return Result{}, err
	}
	defer file.Close()
	st, err := file.Stat()
	if err != nil || !st.Mode().IsRegular() || st.Size() > s.cfg.MaxUploadBytes {
		return Result{}, errArg("Upload must be a bounded regular file")
	}
	private, err := s.reserveUpload(p, st.Size())
	if err != nil {
		return Result{}, err
	}
	retained := false
	defer func() {
		if !retained {
			s.releaseUpload(private)
		}
	}()
	stage, err := os.OpenFile(filepath.Join(private, filepath.Base(source)), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return Result{}, err
	}
	defer stage.Close()
	copied, err := copyChecked(ctx, stage, io.LimitReader(file, st.Size()+1))
	if err != nil {
		return Result{}, err
	}
	if copied != st.Size() {
		return Result{}, wire.Fail("source_changed", "Upload source size changed")
	}
	if err = stage.Close(); err != nil {
		return Result{}, err
	}
	err = p.write(ctx, func() error {
		object, err := p.resolve(ctx, a.Locator)
		if err != nil {
			return err
		}
		defer p.release(object)
		// A lost response can still mean Chrome accepted the file. Keep its
		// backing bytes even on an uncertain CDP result, until this page closes.
		s.mu.Lock()
		_, retained = s.uploads[private]
		s.mu.Unlock()
		if !retained {
			return wire.Fail("closed", "Upload page closed")
		}
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

func (s *Service) DownloadRead(ctx context.Context, a DownloadReadArgs) (DownloadPart, error) {
	d, err := s.DownloadGet(ctx, a.DownloadArgs)
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

// evictDownloads 容量淘汰（纯容量语义，无墙钟）：下载只随页面关闭与容量压力
// 回收。触发点 = 新下载登记（downloadWillBegin）：按创建先后淘汰最旧的非进行中
// 下载（删文件+记录），直到记录数低于上限且总字节为新下载预留出一份单文件上限；
// inProgress 永不被动淘汰——进行中超量由 downloadEvent 的
// MaxDownloadBytes/MaxTotalDownloadBytes 硬检查取消。选登记时而非定时器：容量
// 压力只在新下载到来时产生，定时清扫在无压力时也删记录，语义更重。
// Called with s.mu held.
func (s *Service) evictDownloads() {
	for {
		var total int64
		for _, d := range s.downloads {
			total += d.Bytes
		}
		if len(s.downloads) < maxDownloads && total+s.cfg.MaxDownloadBytes <= s.cfg.MaxTotalDownloadBytes {
			return
		}
		var oldest *download
		for _, d := range s.downloads {
			if d.State == "inProgress" {
				continue
			}
			if oldest == nil || d.created.Before(oldest.created) {
				oldest = d
			}
		}
		if oldest == nil {
			return
		}
		_ = os.Remove(oldest.path)
		delete(s.downloads, oldest.ID)
	}
}
