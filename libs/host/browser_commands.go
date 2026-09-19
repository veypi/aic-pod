package host

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"sync"
	"time"

	"github.com/veypi/aic-pod/libs/hostcmd"
	"github.com/veypi/aic-pod/protocol/hosts"
)

// Browser command execution uses the same device provider as AI automation.
// Neither the RTC transport nor its frontend knows about Electron IPC.
func browserDescriptor() hosts.Command {
	methods := map[string]hosts.Method{}
	methods["list"] = hosts.Method{Mode: "request", Effect: "read", InputSchema: json.RawMessage(`{"type":"object"}`)}
	methods["view"] = hosts.Method{Mode: "live", Effect: "write", InputSchema: json.RawMessage(`{"type":"object"}`)}
	for _, name := range []string{"create", "navigate", "close", "back", "forward", "reload", "dialog"} {
		methods[name] = hosts.Method{Effect: "write", InputSchema: json.RawMessage(`{"type":"object"}`)}
	}
	return hosts.Command{Name: "browser", Contract: "browser/1", Methods: methods}
}
func directShell(ctx context.Context, ch ShellChannel, call hostcmd.Call) (json.RawMessage, error) {
	conn, err := (&net.Dialer{Timeout: 3 * time.Second}).DialContext(ctx, "tcp", ch.Addr)
	if err != nil {
		return nil, hosts.Fail("unreachable", "Browser provider is unavailable")
	}
	defer conn.Close()
	deadline := time.Now().Add(10 * time.Second)
	if end, ok := ctx.Deadline(); ok && end.Before(deadline) {
		deadline = end
	}
	conn.SetDeadline(deadline)
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	request := struct {
		Token      string          `json:"token"`
		Direct     bool            `json:"direct"`
		Session    string          `json:"session_id"`
		Connection string          `json:"connection_id"`
		Method     string          `json:"method"`
		Args       json.RawMessage `json:"args"`
		Deadline   int64           `json:"deadline_ms"`
	}{ch.Token, true, call.SessionID, call.Caller.ConnectionID, call.Method, call.Args, deadline.UnixMilli()}
	if err = json.NewEncoder(conn).Encode(request); err != nil {
		f := hosts.Fail("unreachable", "Browser request delivery failed")
		f.Effect = "unknown"
		return nil, f
	}
	var response struct {
		Result json.RawMessage `json:"result"`
		Error  *hosts.Fault    `json:"error"`
	}
	// Small command responses only; live frames use the duplex provider connection.
	if err = json.NewDecoder(bufio.NewReader(io.LimitReader(conn, 16<<20))).Decode(&response); err != nil {
		f := hosts.Fail("unreachable", "Browser response was lost")
		f.Effect = "unknown"
		return nil, f
	}
	if response.Error != nil {
		return nil, response.Error
	}
	if len(response.Result) == 0 {
		f := hosts.Fail("unreachable", "Invalid browser provider response")
		f.Effect = "unknown"
		return nil, f
	}
	return response.Result, nil
}

type browserCommands struct{}

func (b *browserCommands) provider() hostcmd.Provider {
	return hostcmd.Provider{Descriptor: browserDescriptor(), Validate: func(method string, args json.RawMessage) error {
		_, err := hosts.CanonicalArgs(args)
		return err
	}, Run: func(ctx context.Context, call hostcmd.Call) (any, error) {
		p, ok := lookupProvider("browser")
		if !ok || p.Direct == nil {
			return nil, hosts.Fail("unsupported", "Device has no browser provider")
		}
		return directShell(ctx, *p.Direct, call)
	}, OpenLive: func(ctx context.Context, call hostcmd.Call) (hostcmd.Live, error) {
		p, ok := lookupProvider("browser")
		if !ok || p.Direct == nil {
			return nil, hosts.Fail("unsupported", "Device has no browser provider")
		}
		return openBrowserLive(ctx, *p.Direct, call)
	}}
}
func (b *browserCommands) closeSession(id string) {
	if p, ok := lookupProvider("browser"); ok && p.Direct != nil {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		directShell(ctx, *p.Direct, hostcmd.Call{SessionID: id, Method: "session.release", Args: json.RawMessage(`{}`)})
	}
}
func (b *browserCommands) disconnect(connection string) {
	if p, ok := lookupProvider("browser"); ok && p.Direct != nil {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		directShell(ctx, *p.Direct, hostcmd.Call{Caller: hostcmd.Caller{ConnectionID: connection}, Method: "connection.release", Args: json.RawMessage(`{}`)})
	}
}

type browserLive struct {
	conn   net.Conn
	reader *bufio.Reader
	send   sync.Mutex
	stop   func() bool
}

func openBrowserLive(ctx context.Context, ch ShellChannel, call hostcmd.Call) (hostcmd.Live, error) {
	conn, err := (&net.Dialer{Timeout: 3 * time.Second}).DialContext(ctx, "tcp", ch.Addr)
	if err != nil {
		return nil, hosts.Fail("unreachable", "Browser provider is unavailable")
	}
	l := &browserLive{conn: conn, reader: bufio.NewReaderSize(conn, hosts.MaxControlBytes)}
	l.stop = context.AfterFunc(ctx, func() { conn.Close() })
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	err = json.NewEncoder(conn).Encode(map[string]any{"token": ch.Token, "direct": true, "live": true,
		"session_id": call.SessionID, "connection_id": call.Caller.ConnectionID, "method": call.Method, "args": call.Args,
		"deadline_ms": time.Now().Add(10 * time.Second).UnixMilli()})
	if err == nil {
		var line []byte
		line, err = l.reader.ReadSlice('\n')
		if err == nil {
			var response struct {
				Result struct {
					Ready bool `json:"ready"`
				} `json:"result"`
				Error *hosts.Fault `json:"error"`
			}
			err = json.Unmarshal(line, &response)
			if response.Error != nil {
				err = response.Error
			} else if err == nil && !response.Result.Ready {
				err = hosts.Fail("unreachable", "Invalid live provider response")
			}
		}
	}
	if err != nil {
		l.Close()
		return nil, err
	}
	conn.SetDeadline(time.Time{})
	return l, nil
}
func (l *browserLive) write(value any) error {
	l.send.Lock()
	defer l.send.Unlock()
	l.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	return json.NewEncoder(l.conn).Encode(value)
}
func (l *browserLive) Recv() (hostcmd.LiveItem, error) {
	var item hostcmd.LiveItem
	if err := l.write(map[string]string{"type": "pull"}); err != nil {
		return item, err
	}
	var size [4]byte
	if _, err := io.ReadFull(l.reader, size[:]); err != nil {
		return item, err
	}
	n := binary.BigEndian.Uint32(size[:])
	if n < 2 || n > hosts.MaxHeaderBytes {
		return item, hosts.Fail("overloaded", "Live metadata exceeds limit")
	}
	header := make([]byte, n)
	if _, err := io.ReadFull(l.reader, header); err != nil {
		return item, err
	}
	var meta struct {
		Metadata json.RawMessage `json:"metadata"`
		Size     int             `json:"size"`
	}
	if err := hosts.Decode(header, &meta); err != nil {
		return item, err
	}
	if meta.Size < 0 || meta.Size > hostcmd.MaxLiveBytes {
		return item, hosts.Fail("overloaded", "Live item exceeds limit")
	}
	item.Metadata = meta.Metadata
	item.Data = make([]byte, meta.Size)
	_, err := io.ReadFull(l.reader, item.Data)
	return item, err
}
func (l *browserLive) Send(data json.RawMessage) error {
	return l.write(map[string]any{"type": "input", "data": data})
}
func (l *browserLive) Close() error { l.stop(); return l.conn.Close() }
