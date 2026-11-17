package host

import (
	"context"
	"errors"
	"testing"
)

func TestSwitchAccountDestroysOldClientBeforeConnecting(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	rtMu.Lock()
	if rtClient != nil {
		_ = rtClient.Close()
		rtClient = nil
	}
	rtMu.Unlock()
	t.Cleanup(Stop)

	oldOpts := sessionTestOptions(sessionTestUIDA, func(context.Context) (string, error) {
		return sessionTestJWT(sessionTestUIDA, "access", 0), nil
	})
	oldClient := New(oldOpts)
	rtClient = oldClient
	oldPerms, oldProcs := oldClient.perms, oldClient.procs

	newOpts := sessionTestOptions(sessionTestUIDB, func(context.Context) (string, error) {
		return sessionTestJWT(sessionTestUIDB, "access", 0), nil
	})
	newClient := New(newOpts)
	connect := func(opts Options) (*Client, error) {
		if !oldClient.closed {
			t.Fatal("new account connected before old Client was destroyed")
		}
		if opts.Session.UID != sessionTestUIDB {
			t.Fatalf("new options uid = %q", opts.Session.UID)
		}
		return newClient, nil
	}
	if err := switchAccount(newOpts, connect); err != nil {
		t.Fatal(err)
	}
	if rtClient != newClient || !oldClient.closed || newClient.closed {
		t.Fatal("account switch retained the wrong Client lifecycle")
	}
	if newClient.uid != sessionTestUIDB || newClient.hostID != sessionTestUIDB+"0001" {
		t.Fatalf("new identity = %s/%s", newClient.uid, newClient.hostID)
	}
	if newClient.perms == oldPerms || newClient.procs == oldProcs {
		t.Fatal("account switch reused permission or background process state")
	}
}

func TestSwitchAccountFailureDoesNotResurrectOldClient(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	rtMu.Lock()
	if rtClient != nil {
		_ = rtClient.Close()
		rtClient = nil
	}
	rtMu.Unlock()
	t.Cleanup(Stop)

	oldClient := New(sessionTestOptions(sessionTestUIDA, func(context.Context) (string, error) {
		return sessionTestJWT(sessionTestUIDA, "access", 0), nil
	}))
	rtClient = oldClient
	wantErr := errors.New("new account unavailable")
	err := switchAccount(sessionTestOptions(sessionTestUIDB, func(context.Context) (string, error) {
		return sessionTestJWT(sessionTestUIDB, "access", 0), nil
	}), func(Options) (*Client, error) { return nil, wantErr })
	if !errors.Is(err, wantErr) {
		t.Fatalf("error = %v", err)
	}
	if !oldClient.closed || rtClient != nil {
		t.Fatal("failed account switch retained or resurrected old account")
	}
}
