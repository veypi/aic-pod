package fs

import (
	wire "github.com/veypi/aic-pod/protocol/hosts_tools"
)

type ResourceRef struct {
	ID    string `json:"id"`
	Epoch string `json:"epoch"`
	Kind  string `json:"kind"`
}
type Fault = wire.Fault

const MaxSafeInteger = 1<<53 - 1

func Fail(code, message string) *Fault { f := wire.Fail(code, message); f.Effect = "none"; return f }

var Decode = wire.Decode
var ValidID = wire.ValidID

func NewID(prefix string) (string, error) { return wire.NewID(prefix), nil }

var AsFault = wire.AsFault
