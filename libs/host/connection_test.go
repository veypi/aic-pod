package host

import (
	"bufio"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
)

// Real nats.Conn objects over in-memory pipes exercise concurrent Publish,
// Close and heartbeat shutdown without a server process or external sockets.
type pipeNATSDialer struct{}

func (pipeNATSDialer) Dial(string, string) (net.Conn, error) {
	client, server := net.Pipe()
	go func() {
		defer server.Close()
		if _, err := io.WriteString(server, "INFO {\"max_payload\":1048576}\r\n"); err != nil {
			return
		}
		r := bufio.NewReader(server)
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			fields := strings.Fields(line)
			if len(fields) == 0 {
				continue
			}
			switch fields[0] {
			case "PING":
				if _, err := io.WriteString(server, "PONG\r\n"); err != nil {
					return
				}
			case "PUB":
				n, _ := strconv.Atoi(fields[len(fields)-1])
				if _, err := io.CopyN(io.Discard, r, int64(n+2)); err != nil {
					return
				}
			}
		}
	}()
	return client, nil
}

func TestConnectionReplacementStopsOldHeartbeatAndConcurrentReaders(t *testing.T) {
	c, _ := testClient(t)
	stop := make(chan struct{})
	var readers sync.WaitGroup
	for i := 0; i < 4; i++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if nc := c.connection(); nc != nil {
					_ = nc.Publish("fixture", nil)
				}
			}
		}()
	}
	defer func() { close(stop); readers.Wait() }()
	var previous *nats.Conn
	var oldDone <-chan struct{}
	for i := 0; i < 12; i++ {
		nc, err := nats.Connect("nats://fixture:4222", nats.SetCustomDialer(pipeNATSDialer{}), nats.NoReconnect())
		if err != nil {
			t.Fatal(err)
		}
		c.installConnection(nc)
		if previous != nil {
			if !previous.IsClosed() {
				t.Fatal("old NATS connection remains open")
			}
			select {
			case <-oldDone:
			case <-time.After(time.Second):
				t.Fatal("old heartbeat survived replacement")
			}
		}
		previous, oldDone = nc, c.heartbeatDone
	}
	c.closeConnection()
	select {
	case <-oldDone:
	case <-time.After(time.Second):
		t.Fatal("close retained heartbeat")
	}
	if c.connection() != nil {
		t.Fatal("close retained connection")
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if err := c.Connect(); err == nil {
		t.Fatal("closed client reconnected")
	}
}
