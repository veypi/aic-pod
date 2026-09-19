package host

import (
	"bytes"
	"context"
	"encoding/json"

	"github.com/veypi/aic-pod/protocol/hosts"
)

func (s *CommandService) Limits(proxy bool) map[string]any { return s.transfers.Limits(proxy) }

// HandlePacket is shared by RTC and HTTP/NATS. Only authentication and delivery
// differ between transports; session, file, resource and stream semantics do not.
func (s *CommandService) HandlePacket(ctx context.Context, conn string, packet hosts.Packet) hosts.Packet {
	var req hosts.Request
	var value any
	var err error
	caller, authErr := s.Access.DataCaller(conn)
	if packet.Binary {
		h, data, e := hosts.DecodeFrame(packet.Data)
		if e == nil && (!hosts.ValidID(h.RequestID) || !hosts.ValidID(h.SessionID)) {
			e = hosts.Fail("invalid_argument", "Missing file frame identity")
		}
		if e == nil {
			e = authErr
		}
		if e == nil {
			value, e = s.transfers.Write(conn, h, data, caller.Transport == "proxy")
		}
		return hosts.JSONPacket(hosts.Reply(h.RequestID, value, e))
	}
	req, err = hosts.ParseRequest(packet.Data)
	if err == nil {
		err = authErr
	}
	if err != nil {
		return hosts.JSONPacket(hosts.Reply(req.ID, nil, err))
	}
	switch req.Method {
	case "bytes.create", "bytes.read":
		value, err = s.transfers.Open(conn, caller.Transport == "proxy", req.Method == "bytes.create", req.Params)
	case "bytes.seal":
		value, err = s.transfers.Seal(conn, req.Params)
	case "stream.pull":
		var raw []byte
		raw, err = s.transfers.Pull(ctx, conn, req.Params, req.ID)
		if err == nil {
			return hosts.Packet{Binary: true, Data: raw}
		}
	case "stream.status", "stream.cancel":
		var p struct {
			Session string `json:"session_id"`
			Stream  string `json:"stream_id"`
		}
		err = hosts.Decode(req.Params, &p)
		if err == nil {
			if req.Method == "stream.status" {
				value, err = s.transfers.Status(conn, p.Session, p.Stream)
			} else {
				err = s.transfers.Cancel(conn, p.Session, p.Stream)
				value = map[string]bool{"cancelled": err == nil}
			}
		}
	case "resource.describe", "resource.release":
		var p struct {
			Session string            `json:"session_id"`
			Ref     hosts.ResourceRef `json:"ref"`
		}
		err = hosts.Decode(req.Params, &p)
		if err == nil {
			if req.Method == "resource.describe" {
				value, err = s.DescribeBytes(conn, p.Session, p.Ref)
			} else {
				err = s.ReleaseBytes(conn, p.Session, p.Ref)
				value = map[string]bool{"released": err == nil}
			}
		}
	default:
		r := s.Runtime.Handle(ctx, caller, packet.Data)
		value = r.Result
		if r.Error != nil {
			err = r.Error
		}
	}
	response := hosts.JSONPacket(hosts.Reply(req.ID, value, err))
	if len(response.Data) > hosts.MaxControlBytes && err == nil {
		var args struct {
			Session   string `json:"session_id"`
			Operation string `json:"operation_id"`
		}
		_ = json.Unmarshal(req.Params, &args)
		body, e := json.Marshal(value)
		if e == nil {
			n := int64(len(body))
			source, uploadErr := s.Upload(ctx, conn, args.Session, bytes.NewReader(body), &n, "", "application/json")
			e = uploadErr
			if e == nil && args.Operation != "" {
				verify, policyErr := s.Runtime.ResultPolicy(caller, args.Session, args.Operation)
				e = policyErr
				if e == nil {
					e = s.bytes.SetVerifier(args.Session, source.Ref, verify)
				}
				if e != nil {
					_ = s.bytes.Release(args.Session, source.Ref)
				}
			}
			if e == nil {
				return hosts.JSONPacket(hosts.Reply(req.ID, map[string]any{"result_source": source}, nil))
			}
		}
		f := hosts.Fail("overloaded", "Result requires byte storage")
		f.Retry = "query_operation"
		f.Effect = "unknown"
		response = hosts.JSONPacket(hosts.Reply(req.ID, nil, f))
	}
	return response
}
