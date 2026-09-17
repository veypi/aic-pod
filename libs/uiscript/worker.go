// Package uiscript runs ui/1 orchestration in a disposable JS interpreter.
// Only JSON bridge functions are exposed: no Node, Go reflection, filesystem,
// network, process or driver objects are reachable from script code.
package uiscript

import (
	"bufio"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"runtime"
	"runtime/debug"
	"time"

	"github.com/dop251/goja"
	"github.com/veypi/aic-pod/protocol/ui"
)

const WorkerArg = "__ui_script_worker"
const MaxCode = 512 * 1024
const MaxMessage = 2 * 1024 * 1024
const MaxSteps = 256

//go:embed sdk.js
var sdk string

type Init struct {
	Domain string `json:"domain"`
	Code   string `json:"code"`
}
type Message struct {
	Kind  string          `json:"kind"`
	Argv  []string        `json:"argv,omitempty"`
	Value json.RawMessage `json:"value,omitempty"`
	Error *ui.Error       `json:"error,omitempty"`
	Step  int             `json:"step,omitempty"`
}

// Main is called before the ordinary CLI starts. The parent owns wall time and
// OS confinement. The private worker additionally watches its Go heap; this is
// a soft memory guard, not a claim of a hard per-allocation memory quota.
func Main() int {
	debug.SetMemoryLimit(96 << 20)
	go func() {
		for range time.Tick(20 * time.Millisecond) {
			var stats runtime.MemStats
			runtime.ReadMemStats(&stats)
			if stats.HeapAlloc > 128<<20 {
				fmt.Fprintln(os.Stderr, "ui-script memory limit exceeded")
				os.Exit(125)
			}
		}
	}()
	if err := Serve(os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

func Serve(input io.Reader, output io.Writer) error {
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 4096), MaxMessage)
	if !scanner.Scan() {
		return fmt.Errorf("missing script initialization")
	}
	var init Init
	if err := json.Unmarshal(scanner.Bytes(), &init); err != nil {
		return err
	}
	if len(init.Code) > MaxCode || (init.Domain != "browser" && init.Domain != "cua") {
		return fmt.Errorf("invalid script initialization")
	}
	encoder := json.NewEncoder(output)
	vm := goja.New()
	vm.SetMaxCallStackSize(1024)
	unhandled := map[*goja.Promise]bool{}
	vm.SetPromiseRejectionTracker(func(p *goja.Promise, operation goja.PromiseRejectionOperation) {
		if operation == goja.PromiseRejectionReject {
			unhandled[p] = true
		} else {
			delete(unhandled, p)
		}
	})
	type request struct {
		argv    []string
		resolve func(interface{}) error
	}
	var queued *request
	bridge := vm.ToValue(func(call goja.FunctionCall) goja.Value {
		if queued != nil {
			panic(vm.NewTypeError("only one UI call may be pending"))
		}
		var argv []string
		raw := call.Argument(0).String()
		if len(raw) > MaxMessage/2 || json.Unmarshal([]byte(raw), &argv) != nil {
			panic(vm.NewTypeError("invalid UI argv"))
		}
		promise, resolve, _ := vm.NewPromise()
		queued = &request{argv: argv, resolve: resolve}
		return vm.ToValue(promise)
	})
	logBytes := 0
	logger := vm.ToValue(func(call goja.FunctionCall) goja.Value {
		raw := call.Argument(0).String()
		logBytes += len(raw)
		if logBytes > 64*1024 {
			panic(vm.NewTypeError("script log exceeds 64 KiB"))
		}
		if err := encoder.Encode(Message{Kind: "log", Value: json.RawMessage(raw)}); err != nil {
			panic(vm.NewGoError(err))
		}
		return goja.Undefined()
	})
	createValue, err := vm.RunScript("ui-sdk.js", sdk)
	if err != nil {
		return err
	}
	create, _ := goja.AssertFunction(createValue)
	schemaJSON, _ := json.Marshal(ui.Schema)
	// JSON avoids exposing Go maps/structs and their reflection wrappers.
	schema, err := vm.RunString("JSON.parse(" + fmt.Sprintf("%q", string(schemaJSON)) + ")")
	if err != nil {
		return err
	}
	apiValue, err := create(goja.Undefined(), bridge, logger, vm.ToValue(init.Domain), schema)
	if err != nil {
		return err
	}
	api := apiValue.ToObject(vm)
	encode, _ := goja.AssertFunction(api.Get("encode"))
	errorJSON, _ := goja.AssertFunction(api.Get("error"))
	finishError := func(value goja.Value) error {
		encoded, e := errorJSON(goja.Undefined(), value)
		if e != nil {
			return e
		}
		var detail struct {
			Code    string
			Message string
			Step    int
		}
		if e := json.Unmarshal([]byte(encoded.String()), &detail); e != nil {
			return e
		}
		return encoder.Encode(Message{Kind: "done", Error: ui.Err(detail.Code, detail.Message), Step: detail.Step})
	}
	constructorValue, err := vm.RunScript("ui-run.js", "Object.getPrototypeOf(async function(){}).constructor")
	if err != nil {
		return err
	}
	constructor, _ := goja.AssertConstructor(constructorValue)
	fnValue, err := constructor(nil, vm.ToValue("ui"), vm.ToValue("console"), vm.ToValue("'use strict';\n"+init.Code))
	if err != nil {
		return encoder.Encode(Message{Kind: "done", Error: ui.Err("script_error", err.Error())})
	}
	fn, _ := goja.AssertFunction(fnValue)
	value, err := fn(goja.Undefined(), api.Get("ui"), api.Get("console"))
	if err != nil {
		return encoder.Encode(Message{Kind: "done", Error: ui.Err("script_error", err.Error())})
	}
	promise, ok := value.Export().(*goja.Promise)
	if !ok {
		return fmt.Errorf("script did not return an async result")
	}
	for promise.State() == goja.PromiseStatePending {
		for rejected := range unhandled {
			if rejected != promise {
				return finishError(rejected.Result())
			}
		}
		if queued == nil {
			return encoder.Encode(Message{Kind: "done", Error: ui.Err("unresolved_promise", "script is waiting without an outstanding ui call; use ui.wait for delays")})
		}
		next := queued
		queued = nil
		if err := encoder.Encode(Message{Kind: "call", Argv: next.argv}); err != nil {
			return err
		}
		if !scanner.Scan() {
			return fmt.Errorf("UI bridge closed: %v", scanner.Err())
		}
		if err := next.resolve(string(scanner.Bytes())); err != nil {
			return err
		}
	}
	if promise.State() == goja.PromiseStateRejected {
		return finishError(promise.Result())
	}
	for rejected := range unhandled {
		return finishError(rejected.Result())
	}
	if queued != nil {
		return encoder.Encode(Message{Kind: "done", Error: ui.Err("unawaited_call", "script returned with an unawaited ui call; no further steps were dispatched")})
	}
	value, err = encode(goja.Undefined(), promise.Result())
	if err != nil {
		return encoder.Encode(Message{Kind: "done", Error: ui.Err("invalid_result", err.Error())})
	}
	if queued != nil {
		return encoder.Encode(Message{Kind: "done", Error: ui.Err("unawaited_call", "result serialization attempted a UI call")})
	}
	raw := value.String()
	if goja.IsUndefined(value) {
		raw = "null"
	}
	if len(raw) > 1024*1024 {
		return encoder.Encode(Message{Kind: "done", Error: ui.Err("resource_limit", "script return value exceeds 1 MiB")})
	}
	return encoder.Encode(Message{Kind: "done", Value: json.RawMessage(raw)})
}
