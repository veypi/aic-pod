package host

import (
	"context"
	"io"
	"net/http"
	"time"
)

// httpFetcher 是物理 host 的 curl fetcher（§5.4：不限制 SSRF——
// 用户本机网络属其自身边界）。
type httpFetcher struct{}

// Get 实现 vcore.Fetcher。
func (httpFetcher) Get(ctx context.Context, rawurl string) (io.ReadCloser, int64, error) {
	client := &http.Client{Timeout: 10 * time.Minute}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawurl, nil)
	if err != nil {
		return nil, 0, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		resp.Body.Close()
		return nil, 0, errFromStatus(resp.Status)
	}
	return resp.Body, resp.ContentLength, nil
}

func errFromStatus(status string) error {
	return &httpError{status}
}

type httpError struct{ status string }

func (e *httpError) Error() string { return "fetch returned " + e.status }
