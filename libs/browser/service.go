package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/veypi/aic-pod/libs/browser/chrome"
	tool "github.com/veypi/aic-pod/libs/hosts_tool"
	wire "github.com/veypi/aic-pod/protocol/hosts_tools"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

type page struct {
	castMu                          sync.Mutex
	mu                              sync.Mutex
	gate                            chan struct{}
	info                            PageInfo
	target, session, subject, frame string
	conn                            *chrome.Conn
	refs                            map[string]reference
	events                          []Event
	cursor                          uint64
	closed                          bool
	inputs                          map[*inputStream]bool
	lease                           *lease
	viewers                         map[*frameStream]bool
	casting                         bool
	frameSeq                        uint64
	frameTimestamp                  float64
	controlEpoch                    uint64
}

func (p *page) snapshot() PageInfo {
	p.mu.Lock()
	defer p.mu.Unlock()
	v := p.info
	if v.Dialog != nil {
		copy := *v.Dialog
		v.Dialog = &copy
	}
	return v
}
func (p *page) event(kind string, data any) {
	p.cursor++
	p.events = append(p.events, Event{Cursor: p.cursor, Kind: kind, Data: data})
	if len(p.events) > 512 {
		p.events = append([]Event(nil), p.events[len(p.events)-512:]...)
	}
}
func (p *page) call(ctx context.Context, method string, params, result any) error {
	return p.conn.Call(ctx, p.session, method, params, result)
}

type Service struct {
	unlock    func()
	owner     string
	mu        sync.Mutex
	start     chan struct{}
	cfg       Config
	conn      *chrome.Conn
	pages     map[string]*page
	targets   map[string]*page
	sessions  map[string]*page
	frames    map[string]*page
	downloads map[string]*download
	uploads   map[string]upload
	ctx       context.Context
	cancel    context.CancelFunc
	closed    bool
	lastError string
}

func New(cfg Config) *Service {
	if cfg.Width == 0 {
		cfg.Width = 1280
	}
	if cfg.Height == 0 {
		cfg.Height = 720
	}
	if cfg.MaxPages <= 0 {
		cfg.MaxPages = 32
	}
	if cfg.MaxDownloadBytes <= 0 {
		cfg.MaxDownloadBytes = 256 << 20
	}
	if cfg.MaxTotalDownloadBytes <= 0 {
		cfg.MaxTotalDownloadBytes = 1 << 30
	}
	if cfg.DownloadTTL <= 0 {
		cfg.DownloadTTL = time.Hour
	}
	if cfg.MaxUploads <= 0 {
		cfg.MaxUploads = 128
	}
	if cfg.MaxUploadBytes <= 0 {
		cfg.MaxUploadBytes = 256 << 20
	}
	if cfg.MaxTotalUploadBytes <= 0 {
		cfg.MaxTotalUploadBytes = 1 << 30
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Service{cfg: cfg, start: make(chan struct{}, 1), pages: map[string]*page{}, targets: map[string]*page{}, sessions: map[string]*page{}, frames: map[string]*page{}, downloads: map[string]*download{}, uploads: map[string]upload{}, ctx: ctx, cancel: cancel}
	go func() {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.mu.Lock()
				s.pruneDownloads()
				s.mu.Unlock()
			}
		}
	}()
	return s
}

// Configure affects new pages; executable changes take effect on the next Chrome launch.
func (s *Service) Configure(path string, width, height int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cfg.Path = path
	if width >= 320 && width <= 4096 {
		s.cfg.Width = width
	}
	if height >= 320 && height <= 4096 {
		s.cfg.Height = height
	}
}
func (s *Service) settings() Config { s.mu.Lock(); defer s.mu.Unlock(); return s.cfg }
func (s *Service) logf(format string, args ...any) {
	s.mu.Lock()
	fn := s.cfg.Logf
	s.mu.Unlock()
	if fn != nil {
		fn(format, args...)
	}
}
func (s *Service) Status(ctx context.Context, c tool.Caller, a Empty) (map[string]any, error) {
	settings := s.settings()
	path, err := chrome.Resolve(settings.Path)
	state := "stopped"
	s.mu.Lock()
	if s.conn != nil {
		state = "ready"
	}
	last := s.lastError
	s.mu.Unlock()
	if err != nil {
		state = "unavailable"
		last = err.Error()
	}
	return map[string]any{"state": state, "executable": path, "error": last, "viewport": map[string]int{"width": settings.Width, "height": settings.Height}}, nil
}
func (s *Service) ensure(ctx context.Context) (*chrome.Conn, error) {
	select {
	case s.start <- struct{}{}:
		defer func() { <-s.start }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, wire.Fail("closed", "Browser closed")
	}
	if s.conn != nil {
		c := s.conn
		s.mu.Unlock()
		return c, nil
	}
	s.mu.Unlock()
	settings := s.settings()
	path, err := chrome.Resolve(settings.Path)
	if err != nil {
		return nil, wire.Fail("unavailable", err.Error())
	}
	if s.cfg.StateDir == "" {
		return nil, fmt.Errorf("browser state directory is required")
	}
	s.mu.Lock()
	if s.unlock == nil {
		unlock, err := chrome.Lock(s.cfg.StateDir)
		if err != nil {
			s.mu.Unlock()
			return nil, wire.Fail("profile_busy", "Browser state is in use by another process")
		}
		s.unlock = unlock
	}
	s.downloads = map[string]*download{}
	s.mu.Unlock()
	// No upload belongs to a page in this new Chrome process. Discard private
	// stages left by a crash before accepting new file inputs.
	if err = os.RemoveAll(filepath.Join(s.cfg.StateDir, "uploads")); err != nil {
		return nil, err
	}
	stage := filepath.Join(s.cfg.StateDir, "downloads")
	if err = os.MkdirAll(stage, 0700); err != nil {
		return nil, err
	}
	// The private stage has no durable public IDs; leftovers from an earlier process are discarded.
	entries, err := os.ReadDir(stage)
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			_ = os.Remove(filepath.Join(stage, entry.Name()))
		}
	}
	startup, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	conn, err := chrome.Start(startup, path, filepath.Join(s.cfg.StateDir, "profile"))
	if err != nil {
		s.logf("browser: chrome launch failed: %v", err)
		s.mu.Lock()
		s.lastError = err.Error()
		s.mu.Unlock()
		return nil, wire.Fail("unavailable", err.Error())
	}
	conn.SetLogf(s.logf)
	s.logf("browser: chrome ready (pid %d, %s)", conn.Pid(), path)
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		conn.Close()
		return nil, wire.Fail("closed", "Browser closed")
	}
	s.conn = conn
	s.lastError = ""
	s.mu.Unlock()
	go s.events(conn)
	if err = conn.Call(startup, "", "Browser.setDownloadBehavior", map[string]any{"behavior": "allowAndName", "downloadPath": stage, "eventsEnabled": true}, nil); err == nil {
		err = conn.Call(startup, "", "Target.setDiscoverTargets", map[string]any{"discover": true}, nil)
	}
	if err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}
func (s *Service) get(c tool.Caller, id string) (*page, error) {
	if !wire.ValidID(id) {
		return nil, errArg("Invalid page_id")
	}
	s.mu.Lock()
	p := s.pages[id]
	s.mu.Unlock()
	if p == nil || p.subject != c.Subject {
		return nil, wire.Fail("not_found", "Page unavailable to caller")
	}
	p.mu.Lock()
	closed := p.closed
	p.mu.Unlock()
	if closed {
		return nil, wire.Fail("not_found", "Page closed")
	}
	return p, nil
}
func (s *Service) List(ctx context.Context, c tool.Caller, a Empty) ([]PageInfo, error) {
	s.mu.Lock()
	list := []*page{}
	for _, p := range s.pages {
		if p.subject == c.Subject {
			list = append(list, p)
		}
	}
	s.mu.Unlock()
	out := []PageInfo{}
	for _, p := range list {
		out = append(out, p.snapshot())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}
func (s *Service) Create(ctx context.Context, c tool.Caller, a CreateArgs) (PageInfo, error) {
	s.mu.Lock()
	if s.owner != "" && s.owner != c.Subject {
		s.mu.Unlock()
		return PageInfo{}, wire.Fail("permission_denied", "This profile belongs to another actor")
	}
	s.owner = c.Subject
	s.mu.Unlock()
	conn, err := s.ensure(ctx)
	if err != nil {
		return PageInfo{}, err
	}
	s.mu.Lock()
	full := len(s.pages) >= s.cfg.MaxPages
	s.mu.Unlock()
	if full {
		return PageInfo{}, wire.Fail("overloaded", "Page limit reached")
	}
	var target struct {
		ID string `json:"targetId"`
	}
	if err = conn.Call(ctx, "", "Target.createTarget", map[string]any{"url": "about:blank", "newWindow": true}, &target); err != nil {
		return PageInfo{}, err
	}
	p, err := s.attach(ctx, conn, target.ID, c.Subject, a.Width, a.Height)
	if err != nil {
		cleanup, cancel := context.WithTimeout(s.ctx, 3*time.Second)
		defer cancel()
		_ = conn.Call(cleanup, "", "Target.closeTarget", map[string]any{"targetId": target.ID}, nil)
		return PageInfo{}, err
	}
	if a.URL != "" && a.URL != "about:blank" {
		if _, err = s.Navigate(ctx, c, NavigateArgs{PageID: p.info.ID, URL: a.URL}); err != nil {
			return p.snapshot(), err
		}
	}
	return p.snapshot(), nil
}
func (s *Service) attach(ctx context.Context, conn *chrome.Conn, target, subject string, width, height int) (*page, error) {
	settings := s.settings()
	if width == 0 {
		width = settings.Width
	}
	if height == 0 {
		height = settings.Height
	}
	p := &page{target: target, subject: subject, conn: conn, gate: make(chan struct{}, 1), info: PageInfo{ID: wire.NewID("p_"), Document: wire.NewID("doc_"), URL: "about:blank", Width: width, Height: height}, refs: map[string]reference{}, viewers: map[*frameStream]bool{}}
	s.mu.Lock()
	if old := s.targets[target]; old != nil {
		s.mu.Unlock()
		return old, nil
	}
	if len(s.targets) >= s.cfg.MaxPages || s.closed || s.conn != conn {
		s.mu.Unlock()
		return nil, wire.Fail("overloaded", "Browser unavailable or page limit reached")
	}
	s.targets[target] = p
	s.mu.Unlock()
	var attached struct {
		Session string `json:"sessionId"`
	}
	if err := conn.Call(ctx, "", "Target.attachToTarget", map[string]any{"targetId": target, "flatten": true}, &attached); err != nil {
		s.remove(p)
		return nil, err
	}
	s.mu.Lock()
	p.session = attached.Session
	s.sessions[attached.Session] = p
	s.mu.Unlock()
	for _, method := range []string{"Page.enable", "Runtime.enable", "DOM.enable", "Network.enable", "Accessibility.enable"} {
		if err := p.call(ctx, method, map[string]any{}, nil); err != nil {
			s.remove(p)
			return nil, err
		}
	}
	if err := conn.ConfigurePage(ctx, target, attached.Session, width, height); err != nil {
		s.remove(p)
		return nil, err
	}
	var tree struct {
		Tree struct {
			Frame struct{ ID, URL string } `json:"frame"`
		} `json:"frameTree"`
	}
	if err := p.call(ctx, "Page.getFrameTree", map[string]any{}, &tree); err != nil {
		s.remove(p)
		return nil, err
	}
	s.mu.Lock()
	s.frames[tree.Tree.Frame.ID] = p
	s.mu.Unlock()
	p.mu.Lock()
	p.frame = tree.Tree.Frame.ID
	p.info.URL = tree.Tree.Frame.URL
	p.mu.Unlock()
	s.mu.Lock()
	if s.closed || s.conn != conn || s.targets[target] != p {
		s.mu.Unlock()
		s.remove(p)
		return nil, wire.Fail("closed", "Browser closed during attachment")
	}
	s.pages[p.info.ID] = p
	s.mu.Unlock()
	return p, nil
}
func (s *Service) remove(p *page) {
	s.mu.Lock()
	uploads := []string{}
	for dir, u := range s.uploads {
		if u.page == p {
			uploads = append(uploads, dir)
			delete(s.uploads, dir)
		}
	}
	delete(s.pages, p.info.ID)
	delete(s.targets, p.target)
	delete(s.sessions, p.session)
	for f, v := range s.frames {
		if v == p {
			delete(s.frames, f)
		}
	}
	s.mu.Unlock()
	for _, dir := range uploads {
		_ = os.RemoveAll(dir)
	}
	p.mu.Lock()
	p.closed = true
	viewers := []*frameStream{}
	for v := range p.viewers {
		viewers = append(viewers, v)
	}
	inputs := []*inputStream{}
	for i := range p.inputs {
		inputs = append(inputs, i)
	}
	p.lease = nil
	p.mu.Unlock()
	for _, v := range viewers {
		v.Close()
	}
	for _, i := range inputs {
		i.Close()
	}
}
func (s *Service) ClosePage(ctx context.Context, c tool.Caller, a PageArgs) (map[string]bool, error) {
	p, err := s.get(c, a.PageID)
	if err != nil {
		return nil, err
	}
	if err = p.controlAllowed(c); err != nil {
		return nil, err
	}
	err = p.conn.Call(ctx, "", "Target.closeTarget", map[string]any{"targetId": p.target}, nil)
	if err == nil {
		s.remove(p)
	}
	return map[string]bool{"closed": err == nil}, err
}
func (s *Service) Close() error {
	s.start <- struct{}{}
	defer func() { <-s.start }()
	defer func() {
		s.mu.Lock()
		unlock := s.unlock
		s.unlock = nil
		s.mu.Unlock()
		if unlock != nil {
			unlock()
		}
	}()
	s.mu.Lock()
	s.closed = true
	conn := s.conn
	pages := []*page{}
	for _, p := range s.pages {
		pages = append(pages, p)
	}
	s.mu.Unlock()
	s.cancel()
	for _, p := range pages {
		s.remove(p)
	}
	if conn != nil {
		s.logf("browser: closing chrome (pid %d)", conn.Pid())
		return conn.Close()
	}
	return nil
}
func (s *Service) events(conn *chrome.Conn) {
	defer func() {
		s.mu.Lock()
		if s.conn != conn {
			s.mu.Unlock()
			return
		}
		s.conn = nil
		pages := []*page{}
		for _, p := range s.pages {
			pages = append(pages, p)
		}
		cause := conn.Err()
		if cause != nil {
			s.lastError = "Chrome disconnected: " + cause.Error() + "; page IDs expired"
		} else {
			s.lastError = "Chrome disconnected; page IDs expired"
		}
		s.mu.Unlock()
		s.logf("browser: chrome connection ended (pid %d): %v", conn.Pid(), cause)
		for _, p := range pages {
			s.remove(p)
		}
	}()
	for {
		select {
		case <-conn.Done():
			return
		case <-s.ctx.Done():
			return
		case e := <-conn.Events():
			s.onEvent(conn, e)
		}
	}
}
func (s *Service) onEvent(conn *chrome.Conn, e chrome.Event) {
	if e.Method == "Browser.downloadWillBegin" || e.Method == "Browser.downloadProgress" {
		s.downloadEvent(conn, e)
		return
	}
	if e.Method == "Target.targetDestroyed" {
		var v struct {
			Target string `json:"targetId"`
		}
		_ = json.Unmarshal(e.Params, &v)
		s.mu.Lock()
		p := s.targets[v.Target]
		s.mu.Unlock()
		if p != nil {
			s.remove(p)
		}
		return
	}
	if e.Method == "Target.targetCreated" || e.Method == "Target.targetInfoChanged" {
		var event struct {
			Info struct {
				ID     string `json:"targetId"`
				Type   string `json:"type"`
				URL    string `json:"url"`
				Title  string `json:"title"`
				Opener string `json:"openerId"`
			} `json:"targetInfo"`
		}
		if json.Unmarshal(e.Params, &event) != nil {
			return
		}
		s.mu.Lock()
		p := s.targets[event.Info.ID]
		parent := s.targets[event.Info.Opener]
		s.mu.Unlock()
		if p != nil {
			p.mu.Lock()
			p.info.URL = bounded(event.Info.URL, 8192)
			p.info.Title = bounded(event.Info.Title, 2048)
			p.mu.Unlock()
		} else if event.Info.Type == "page" && parent != nil {
			go func() {
				ctx, cancel := context.WithTimeout(s.ctx, 10*time.Second)
				defer cancel()
				child, err := s.attach(ctx, conn, event.Info.ID, parent.subject, 0, 0)
				if err == nil {
					parent.mu.Lock()
					parent.event("popup", child.snapshot())
					parent.mu.Unlock()
				}
			}()
		}
		return
	}
	s.mu.Lock()
	p := s.sessions[e.Session]
	s.mu.Unlock()
	if p == nil {
		return
	}
	if e.Method == "Page.screencastFrame" {
		s.frameEvent(p, e.Params)
		return
	}
	var value map[string]any
	if json.Unmarshal(e.Params, &value) != nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	switch e.Method {
	case "Page.frameNavigated":
		f, _ := value["frame"].(map[string]any)
		id, _ := f["id"].(string)
		parent, _ := f["parentId"].(string)
		s.mu.Lock()
		s.frames[id] = p
		s.mu.Unlock()
		if parent == "" {
			p.frame = id
			p.info.Document = wire.NewID("doc_")
			p.info.URL, _ = f["url"].(string)
			p.refs = map[string]reference{}
			p.event("navigation", map[string]any{"url": p.info.URL, "document_id": p.info.Document})
		}
	case "Page.javascriptDialogOpening":
		p.info.Dialog = &Dialog{ID: wire.NewID("dlg_"), Type: fmt.Sprint(value["type"]), Message: bounded(fmt.Sprint(value["message"]), 8192)}
		p.event("dialog", *p.info.Dialog)
	case "Page.javascriptDialogClosed":
		p.info.Dialog = nil
		p.event("dialog", map[string]bool{"closed": true})
	case "Runtime.consoleAPICalled":
		p.event("console", bounded(string(e.Params), 8192))
	case "Runtime.exceptionThrown":
		p.event("console", bounded(string(e.Params), 8192))
	case "Network.responseReceived":
		res, _ := value["response"].(map[string]any)
		p.event("network", map[string]any{"url": res["url"], "status": res["status"], "mime_type": res["mimeType"]})
	}
}
