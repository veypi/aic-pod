package hosts

import (
	"bytes"
	"encoding/binary"
	"reflect"
	"testing"
)

func TestBinaryFrameBoundaries(t *testing.T) {
	f := Frame{StreamID: "stream_1", ItemID: "item_1", Seq: MaxSafeInteger, Offset: MaxSafeInteger, Final: true}
	wire, err := EncodeFrame(f, []byte{0, 255, 13, 10})
	if err != nil {
		t.Fatal(err)
	}
	h, p, err := DecodeFrame(wire)
	if err != nil || !reflect.DeepEqual(h, f) || !bytes.Equal(p, []byte{0, 255, 13, 10}) {
		t.Fatalf("roundtrip: %+v %x %v", h, p, err)
	}
	for _, raw := range [][]byte{nil, {0, 0, 0, 255}, {255, 255, 255, 255}, make([]byte, MaxDataBytes+1)} {
		if _, _, err := DecodeFrame(raw); err == nil {
			t.Fatal("malformed frame accepted")
		}
	}
	bad := append([]byte{}, wire...)
	binary.BigEndian.PutUint32(bad, MaxHeaderBytes+1)
	if _, _, err := DecodeFrame(bad); err == nil {
		t.Fatal("oversized header accepted")
	}
	f.Offset++
	if _, err := EncodeFrame(f, nil); err == nil {
		t.Fatal("unsafe integer accepted")
	}
}
