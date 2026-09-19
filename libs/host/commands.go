package host

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/veypi/aic-pod/libs/hostcmd"
	"github.com/veypi/aic-pod/libs/hostfs"
	"github.com/veypi/aic-pod/libs/proto"
	"github.com/veypi/aic-pod/protocol/hosts"
)

// CommandService is the authenticated hosts/1 execution boundary. RTC framing
// is intentionally independent; no channel should call a file provider directly.
type CommandService struct {
	Access    *hostcmd.Access
	Runtime   *hostcmd.Runtime
	bytes     *hostcmd.Bytes
	files     *hostfs.FS
	browser   *browserCommands
	transfers *hostcmd.Transfers
	done      chan struct{}
	closeOnce sync.Once
}

func (s *CommandService) Authorization() *hostcmd.Access { return s.Access }
func (s *CommandService) Epoch() string                  { return s.Runtime.Epoch() }
func (s *CommandService) Reap() {
	for _, id := range s.Access.Expired() {
		s.Disconnect(id)
	}
	s.Runtime.Reap()
	s.transfers.Reap()
}
func (s *CommandService) DescribeBytes(connection, session string, ref hosts.ResourceRef) (hostcmd.ByteSource, error) {
	if _, err := s.session(connection, session); err != nil {
		return hostcmd.ByteSource{}, err
	}
	return s.bytes.Describe(session, ref)
}
func (s *CommandService) ReleaseBytes(connection, session string, ref hosts.ResourceRef) error {
	if _, err := s.session(connection, session); err != nil {
		return err
	}
	return s.bytes.Release(session, ref)
}

// NewCommandService uses the device's real credential and owner identity, never
// a frontend-provided user or an AI session's temporary grants.
func (c *Client) NewCommandService(tempDir string) (*CommandService, error) {
	parts := strings.SplitN(c.opts.Key, ".", 4)
	if len(parts) != 4 {
		return nil, fmt.Errorf("invalid device credential")
	}
	version, err := strconv.ParseUint(parts[1], 10, 64)
	if err != nil {
		return nil, err
	}
	key, err := hosts.DirectKey(parts[2], parts[0])
	if err != nil {
		return nil, err
	}
	proxyKey, err := hosts.ProxyKey(parts[2], parts[0])
	if err != nil {
		return nil, err
	}
	access, err := hostcmd.NewAccess(hostcmd.AccessConfig{HostID: parts[0], UserID: parts[3], CredentialVersion: version, Key: key, ProxyKey: proxyKey})
	if err != nil {
		return nil, err
	}
	store, err := hostcmd.NewBytes(hostcmd.BytesConfig{TempDir: tempDir, MaxSourceBytes: c.opts.Transfers.MaxUploadBytes})
	if err != nil {
		return nil, err
	}
	workdir, err := filepath.Abs(c.opts.WorkDir)
	if err != nil {
		store.Close()
		return nil, err
	}
	roots, home, err := deviceFileRoots(workdir)
	if err != nil {
		store.Close()
		return nil, err
	}
	files, err := hostfs.New(hostfs.Config{Roots: roots, Home: &home, Bytes: store, Check: func(ctx context.Context, call hostcmd.Call, path string, write bool) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		// Management identity authorizes the connection, not the file path.
		// RTC and proxy use the same live device fs_policy, without AI session grants.
		env := c.newEnv("", "")
		env.Granted = proto.LevelApproved
		denied := func(message, reason string) error {
			access := "read"
			if write {
				access = "write"
			}
			fault := hosts.Fail("permission_denied", message)
			fault.Details = map[string]any{"path": filepath.ToSlash(path), "access": access, "reason": reason}
			return fault
		}
		if err := env.CheckPath("fs", filepath.ToSlash(path)); err != nil {
			return denied("File is outside the device boundary", "device_boundary")
		}
		if err := env.CheckPolicy("fs "+call.Method, filepath.ToSlash(path), write); err != nil {
			return denied("File access denied by device policy", "device_policy")
		}
		return nil
	}})
	if err != nil {
		store.Close()
		return nil, err
	}
	browser := &browserCommands{}
	var transfers *hostcmd.Transfers
	authorizeProvider := func(ctx context.Context, call hostcmd.Call) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		switch call.Command {
		case "fs":
			return files.Authorize(ctx, call)
		case "browser":
			// Access binds this connection to the device owner. Human browser
			// control has no filesystem authority or AI-session grant inheritance.
			p, ok := lookupProvider("browser")
			if !ok || p.Direct == nil {
				return hosts.Fail("unsupported", "Browser provider is unavailable")
			}
			if _, ok := browserDescriptor().Methods[call.Method]; !ok {
				return hosts.Fail("unsupported", "Browser method is unavailable")
			}
			return nil
		default:
			return hosts.Fail("unsupported", "Command is unavailable")
		}

	}
	rt, err := hostcmd.New(hostcmd.Config{Reauthorize: authorizeProvider, ResultPolicy: func(call hostcmd.Call) (func(context.Context) error, error) {
		if call.Command == "fs" {
			return files.ResultPolicy(call)
		}
		return func(ctx context.Context) error { return authorizeProvider(ctx, call) }, nil
	}, Authorize: func(ctx context.Context, call hostcmd.Call) error {
		active, err := access.Caller(call.Caller.ConnectionID)
		if err != nil {
			return err
		}
		if active.Subject != call.Caller.Subject {
			return hosts.Fail("unauthorized", "Connection identity mismatch")
		}
		return authorizeProvider(ctx, call)
	}, OnSessionClose: func(id string) {
		if transfers != nil {
			transfers.CloseSession(id)
		}
		browser.closeSession(id)
		store.CloseSession(id)
	}})
	if err != nil {
		files.Close()
		store.Close()
		return nil, err
	}
	if err = rt.Register(files.Provider()); err != nil {
		files.Close()
		store.Close()
		return nil, err
	}
	if p, ok := lookupProvider("browser"); ok && p.Direct != nil {
		if err = rt.Register(browser.provider()); err != nil {
			files.Close()
			store.Close()
			return nil, err
		}
	}
	service := &CommandService{Access: access, Runtime: rt, bytes: store, files: files, browser: browser, done: make(chan struct{})}
	transfers = hostcmd.NewTransfers(store, service.CheckSession, c.opts.Transfers)
	service.transfers = transfers
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				service.Reap()
			case <-service.done:
				return
			}
		}
	}()
	return service, nil
}
func (s *CommandService) CheckSession(connection, session string) error {
	_, err := s.session(connection, session)
	return err
}

func (s *CommandService) Handle(ctx context.Context, connection string, raw []byte) hosts.Response {
	caller, err := s.Access.Caller(connection)
	if err != nil {
		return hosts.Reply("", nil, err)
	}
	return s.Runtime.Handle(ctx, caller, raw)
}
func (s *CommandService) session(connection, id string) (hostcmd.Caller, error) {
	caller, err := s.Access.DataCaller(connection)
	if err != nil {
		return caller, err
	}
	return caller, s.Runtime.CheckSession(caller, id)
}

type checkedReader struct {
	reader io.Reader
	check  func() error
}

func (r checkedReader) Read(p []byte) (int, error) {
	if err := r.check(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

type checkedWriter struct {
	writer io.Writer
	check  func() error
}

func (w checkedWriter) Write(p []byte) (int, error) {
	if err := w.check(); err != nil {
		return 0, err
	}
	return w.writer.Write(p)
}
func (s *CommandService) Upload(ctx context.Context, connection, sessionID string, body io.Reader, size *int64, sha, mediaType string) (hostcmd.ByteSource, error) {
	check := func() error { _, err := s.session(connection, sessionID); return err }
	if err := check(); err != nil {
		return hostcmd.ByteSource{}, err
	}
	source, err := s.bytes.Upload(ctx, sessionID, checkedReader{reader: body, check: check}, size, sha, mediaType)
	if err != nil {
		return source, err
	}
	if err = check(); err != nil {
		s.bytes.Release(sessionID, source.Ref)
		return hostcmd.ByteSource{}, err
	}
	return source, nil
}
func (s *CommandService) ReadBytes(ctx context.Context, connection, sessionID string, ref hosts.ResourceRef, offset int64, length *int64, out io.Writer) error {
	check := func() error { _, err := s.session(connection, sessionID); return err }
	if err := check(); err != nil {
		return err
	}
	return s.bytes.Copy(ctx, sessionID, ref, offset, length, checkedWriter{writer: out, check: check})
}
func (s *CommandService) Disconnect(connection string) {
	s.Access.Close(connection)
	s.Runtime.Disconnect(connection)
	go s.browser.disconnect(connection)
}
func (s *CommandService) Close(ctx context.Context) error {
	s.closeOnce.Do(func() { close(s.done) })
	s.transfers.Close()
	s.Access.RevokeAll()
	if err := s.Runtime.Shutdown(ctx); err != nil {
		return err
	}
	if err := s.files.Close(); err != nil {
		return err
	}
	return s.bytes.Close()
}

func (s *CommandService) OpenLive(ctx context.Context, connection string, in hostcmd.RequestCall) (hostcmd.Live, error) {
	caller, err := s.session(connection, in.SessionID)
	if err != nil {
		return nil, err
	}
	return s.Runtime.OpenLive(ctx, caller, in)
}
