package host

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"time"

	"github.com/veypi/aic-pod/libs/vcore"
)

// httpFetcher 是物理 host 的 curl fetcher（§5.4：不限制 SSRF——
// 用户本机网络属其自身边界）。
type httpFetcher struct{}

// Fetch 实现 vcore.Fetcher。
func (httpFetcher) Fetch(ctx context.Context, req vcore.HTTPReq) (io.ReadCloser, int64, error) {
	client := &http.Client{Timeout: 10 * time.Minute}
	hreq, err := http.NewRequestWithContext(ctx, req.Method, req.URL, bytes.NewReader(req.Body))
	if err != nil {
		return nil, 0, err
	}
	for k, v := range req.Headers {
		hreq.Header.Set(k, v)
	}
	resp, err := client.Do(hreq)
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
