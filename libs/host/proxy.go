package host

import (
	"context"
	"encoding/json"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/veypi/aic-pod/libs/proto"
	"github.com/veypi/aic-pod/protocol/hosts"
)

func (c *Client) handleProxy(msg *nats.Msg) {
	subject, err := proto.ProxySubject(c.uid, c.hostID)
	if err != nil || subject != msg.Subject {
		return
	}
	c.rtcMu.RLock()
	s := c.commands
	c.rtcMu.RUnlock()
	packet := hosts.JSONPacket(hosts.Reply("", nil, hosts.Fail("unreachable", "Device command service is unavailable")))
	if s != nil {
		packet = s.HandleProxy(context.Background(), string(msg.Data))
	}
	raw, _ := json.Marshal(packet)
	_ = msg.Respond(raw)
}

func (s *CommandService) HandleProxy(ctx context.Context, token string) hosts.Packet {
	e, c, err := s.Access.Proxy(token)
	id, method := e.Packet.Identity()
	if err != nil {
		return hosts.JSONPacket(hosts.Reply(id, nil, err))
	}
	ctx, cancel := context.WithDeadline(ctx, time.Unix(e.ExpiresAt, 0))
	defer cancel()
	var result any
	switch method {
	case "hello":
		var req hosts.Request
		req, _ = hosts.ParseRequest(e.Packet.Data)
		var args struct {
			Protocol string `json:"protocol"`
		}
		err = hosts.Decode(req.Params, &args)
		if err == nil && args.Protocol != hosts.Protocol {
			err = hosts.Fail("unsupported_protocol", "Expected hosts/1")
		}
		if err != nil {
			s.Disconnect(c.ConnectionID)
		}
		result = map[string]any{"protocol": hosts.Protocol, "host_id": e.HostID, "connection_id": c.ConnectionID, "runtime_epoch": s.Epoch(), "lease_until": c.ExpiresAt.Unix(), "limits": s.Limits(true)}
	case "auth.renew":
		result = map[string]any{"lease_until": c.ExpiresAt.Unix()}
	case "connection.close":
		s.Disconnect(c.ConnectionID)
		result = map[string]bool{"closed": true}
	default:
		return s.HandlePacket(ctx, c.ConnectionID, e.Packet)
	}
	return hosts.JSONPacket(hosts.Reply(id, result, err))
}
