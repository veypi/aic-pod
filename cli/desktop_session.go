package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/veypi/aic-pod/cfg"
	"github.com/veypi/aic-pod/libs/host"
	"github.com/veypi/vigo/logv"
)

// Desktop 的 token 所有者是 Electron 主进程。desktop-session 子命令把 stdout
// 专用为换行 JSON RPC，stdin 接收回执；该模式日志只走 stderr/文件。
// 不监听端口，不把 access token、refresh token 或 protocol secret 放进 argv/env/config。
type desktopRPCRequest struct {
	ID     string          `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

type desktopRPCResponse struct {
	ID     string          `json:"id"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  string          `json:"error,omitempty"`
}

type desktopRPC struct {
	encoder  *json.Encoder
	decoder  *json.Decoder
	writeMu  sync.Mutex
	mu       sync.Mutex
	pending  map[string]chan desktopRPCResponse
	closed   bool
	closeErr error
	done     chan struct{}
	nextID   atomic.Uint64
	once     sync.Once
}

func newDesktopRPC(in io.Reader, out io.Writer) *desktopRPC {
	r := &desktopRPC{
		encoder: json.NewEncoder(out), decoder: json.NewDecoder(in),
		pending: make(map[string]chan desktopRPCResponse), done: make(chan struct{}),
	}
	go r.readLoop()
	return r
}

func (r *desktopRPC) readLoop() {
	for {
		var response desktopRPCResponse
		if err := r.decoder.Decode(&response); err != nil {
			r.close(err)
			return
		}
		r.mu.Lock()
		ch := r.pending[response.ID]
		delete(r.pending, response.ID)
		r.mu.Unlock()
		if ch != nil {
			ch <- response
		}
	}
}

func (r *desktopRPC) close(err error) {
	r.once.Do(func() {
		if err == nil {
			err = io.EOF
		}
		r.mu.Lock()
		r.closed = true
		r.closeErr = err
		for id, ch := range r.pending {
			delete(r.pending, id)
			ch <- desktopRPCResponse{ID: id, Error: "desktop control channel closed: " + err.Error()}
		}
		r.mu.Unlock()
		close(r.done)
	})
}

func (r *desktopRPC) call(ctx context.Context, method string, params any, result any) error {
	id := fmt.Sprintf("d%d", r.nextID.Add(1))
	ch := make(chan desktopRPCResponse, 1)
	r.mu.Lock()
	if r.closed {
		err := r.closeErr
		r.mu.Unlock()
		return fmt.Errorf("desktop control channel is closed: %w", err)
	}
	r.pending[id] = ch
	r.mu.Unlock()

	var raw json.RawMessage
	if params != nil {
		encoded, err := json.Marshal(params)
		if err != nil {
			r.forget(id)
			return err
		}
		raw = encoded
	}
	r.writeMu.Lock()
	err := r.encoder.Encode(desktopRPCRequest{ID: id, Method: method, Params: raw})
	r.writeMu.Unlock()
	if err != nil {
		r.forget(id)
		r.close(err)
		return fmt.Errorf("write desktop control request: %w", err)
	}

	select {
	case <-ctx.Done():
		r.forget(id)
		return ctx.Err()
	case <-r.done:
		r.forget(id)
		r.mu.Lock()
		err := r.closeErr
		r.mu.Unlock()
		return fmt.Errorf("desktop control channel closed: %w", err)
	case response := <-ch:
		if response.Error != "" {
			return errors.New(response.Error)
		}
		if result != nil && len(response.Result) > 0 {
			if err := json.Unmarshal(response.Result, result); err != nil {
				return fmt.Errorf("decode desktop control response: %w", err)
			}
		}
		return nil
	}
}

func (r *desktopRPC) forget(id string) {
	r.mu.Lock()
	delete(r.pending, id)
	r.mu.Unlock()
}

type desktopSessionProfile struct {
	HostID         string `json:"host_id"`
	UID            string `json:"uid"`
	CredentialVer  uint64 `json:"credential_ver"`
	ProtocolSecret string `json:"protocol_secret"`
}

// runDesktopSession 取得稳定 Host 档案后启动 Session Host。每次 NATS 连接、
// 重连及普通 HTTP 请求都通过 access_token RPC 向主进程取最新 Web access token。
func runDesktopSession() error {
	rpc := newDesktopRPC(os.Stdin, os.Stdout)
	defer rpc.close(nil)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	var profile desktopSessionProfile
	if err := rpc.call(ctx, "session_bootstrap", nil, &profile); err != nil {
		cancel()
		return fmt.Errorf("desktop session bootstrap: %w", err)
	}
	cancel()

	session := &host.SessionConfig{
		HostID: profile.HostID, UID: profile.UID, CredentialVer: profile.CredentialVer,
		ProtocolSecret: profile.ProtocolSecret,
		AccessTokenProvider: func(ctx context.Context) (string, error) {
			var result struct {
				AccessToken string `json:"access_token"`
			}
			if err := rpc.call(ctx, "access_token", nil, &result); err != nil {
				return "", err
			}
			return result.AccessToken, nil
		},
	}
	if err := host.StartSession(*cfg.Global, session); err != nil {
		return err
	}
	defer host.Stop()

	go exitWhenParentGone()
	logv.Info().Msgf("desktop session host started (host=%s)", profile.HostID)
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	logv.Info().Msg("shutting down desktop session...")
	return nil
}
