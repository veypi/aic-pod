package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"testing"
	"time"
)

func newTestDesktopRPC(t *testing.T) (*desktopRPC, *json.Decoder, *json.Encoder) {
	t.Helper()
	clientRead, serverWrite := io.Pipe()
	serverRead, clientWrite := io.Pipe()
	rpc := newDesktopRPC(clientRead, clientWrite)
	t.Cleanup(func() {
		rpc.close(nil)
		_ = clientRead.Close()
		_ = clientWrite.Close()
		_ = serverRead.Close()
		_ = serverWrite.Close()
	})
	return rpc, json.NewDecoder(serverRead), json.NewEncoder(serverWrite)
}

func TestDesktopRPCConcurrentAccessTokenCalls(t *testing.T) {
	rpc, requests, responses := newTestDesktopRPC(t)
	done := make(chan error, 1)
	go func() {
		first := desktopRPCRequest{}
		second := desktopRPCRequest{}
		if err := requests.Decode(&first); err != nil {
			done <- err
			return
		}
		if err := requests.Decode(&second); err != nil {
			done <- err
			return
		}
		// 反序回执验证 pending 请求按 ID 解复用，而不是按调用顺序配对。
		for _, item := range []struct {
			id    string
			token string
		}{{second.ID, "token-b"}, {first.ID, "token-a"}} {
			result, _ := json.Marshal(map[string]string{"access_token": item.token})
			if err := responses.Encode(desktopRPCResponse{ID: item.id, Result: result}); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()

	var wg sync.WaitGroup
	results := make([]string, 2)
	errs := make([]error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var result struct {
				AccessToken string `json:"access_token"`
			}
			errs[i] = rpc.call(context.Background(), "access_token", nil, &result)
			results[i] = result.AccessToken
		}()
	}
	wg.Wait()
	for i := range errs {
		if errs[i] != nil {
			t.Fatal(errs[i])
		}
		if results[i] != "token-a" && results[i] != "token-b" {
			t.Fatalf("unexpected token %q", results[i])
		}
	}
	if results[0] == results[1] {
		t.Fatalf("responses were not correlated by request id: %+v", results)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestDesktopRPCContextCancellationDoesNotBlockOtherCalls(t *testing.T) {
	rpc, requests, responses := newTestDesktopRPC(t)
	serverDone := make(chan error, 1)
	go func() {
		var first desktopRPCRequest
		if err := requests.Decode(&first); err != nil {
			serverDone <- err
			return
		}
		var second desktopRPCRequest
		if err := requests.Decode(&second); err != nil {
			serverDone <- err
			return
		}
		result, _ := json.Marshal(map[string]bool{"ok": true})
		serverDone <- responses.Encode(desktopRPCResponse{ID: second.ID, Result: result})
	}()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := rpc.call(ctx, "cancelled", nil, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled call error = %v", err)
	}
	// 已取消请求可能尚未写出；用一个未取消请求验证通道后续仍可工作。
	var result struct {
		OK bool `json:"ok"`
	}
	callCtx, callCancel := context.WithTimeout(context.Background(), time.Second)
	defer callCancel()
	if err := rpc.call(callCtx, "access_token", nil, &result); err != nil {
		t.Fatal(err)
	}
	if !result.OK {
		t.Fatal("missing successful response")
	}
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
}
