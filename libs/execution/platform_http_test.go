package execution

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	vshnet "github.com/veypi/vsh/network"
)

func TestNetClientPlatformRelativeURLAndAuthPrefixes(t *testing.T) {
	t.Parallel()
	seen := map[string]string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen[r.URL.Path] = r.Header.Get("Authorization")
		fmt.Fprint(w, "ok")
	}))
	defer server.Close()

	client := NewNetClient(NetClientConfig{
		AllowPrivate: true,
		Platform: &PlatformHTTPConfig{
			BaseURL:      server.URL,
			AuthPrefixes: DefaultPlatformAuthPrefixes,
			Inject: func(_ context.Context, req *http.Request) error {
				req.Header.Set("Authorization", "Bearer device-token")
				return nil
			},
		},
	})
	for _, target := range []string{
		"/skills/cloud/demo/api/list",
		"/api/public/time",
		server.URL + "/skills/cloud/demo/manifest",
	} {
		resp, err := client.Do(context.Background(), &vshnet.Request{Method: http.MethodGet, URL: target})
		if err != nil {
			t.Fatalf("Do(%q): %v", target, err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("Do(%q) status = %d", target, resp.StatusCode)
		}
	}
	if seen["/skills/cloud/demo/api/list"] != "Bearer device-token" ||
		seen["/skills/cloud/demo/manifest"] != "Bearer device-token" {
		t.Fatalf("platform auth was not injected: %+v", seen)
	}
	if seen["/api/public/time"] != "" {
		t.Fatalf("auth escaped the injection list: %+v", seen)
	}
}

func TestNetClientPlatformInternalHandler(t *testing.T) {
	t.Parallel()
	type identityKey struct{}
	client := NewNetClient(NetClientConfig{
		Platform: &PlatformHTTPConfig{
			BaseURL:      "http://platform.local",
			AuthPrefixes: DefaultPlatformAuthPrefixes,
			Inject: func(_ context.Context, req *http.Request) error {
				*req = *req.WithContext(context.WithValue(req.Context(), identityKey{}, "u1"))
				return nil
			},
			Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				identity, _ := r.Context().Value(identityKey{}).(string)
				fmt.Fprintf(w, "%s:%s", r.URL.Path, identity)
			}),
		},
	})
	resp, err := client.Do(context.Background(), &vshnet.Request{Method: http.MethodGet, URL: "/skills/cloud/demo/manifest"})
	if err != nil {
		t.Fatal(err)
	}
	if got := string(resp.Body); got != "/skills/cloud/demo/manifest:u1" {
		t.Fatalf("internal response = %q", got)
	}
	resp, err = client.Do(context.Background(), &vshnet.Request{Method: http.MethodGet, URL: "/api/public/time"})
	if err != nil {
		t.Fatal(err)
	}
	if got := string(resp.Body); got != "/api/public/time:" {
		t.Fatalf("identity escaped injection list: %q", got)
	}
}

func TestNetClientPlatformAuthNotForwardedCrossOrigin(t *testing.T) {
	t.Parallel()
	external := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "" {
			t.Errorf("cross-origin redirect forwarded Authorization: %q", got)
		}
		fmt.Fprint(w, "external")
	}))
	defer external.Close()
	platform := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, external.URL+"/skills/cloud/steal", http.StatusFound)
	}))
	defer platform.Close()
	client := NewNetClient(NetClientConfig{
		AllowPrivate: true,
		Platform: &PlatformHTTPConfig{
			BaseURL:      platform.URL,
			AuthPrefixes: DefaultPlatformAuthPrefixes,
			Inject: func(_ context.Context, req *http.Request) error {
				req.Header.Set("Authorization", "Bearer device-token")
				return nil
			},
		},
	})
	resp, err := client.Do(context.Background(), &vshnet.Request{
		Method: http.MethodGet, URL: "/skills/cloud/demo/jump", FollowRedirects: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := string(resp.Body); got != "external" {
		t.Fatalf("redirect response = %q", got)
	}
}
