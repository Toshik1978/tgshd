// Package httpstamp is a thin adapter that fetches a small stamp (such as a
// Unix timestamp) over HTTP. It does no parsing and no judgement: it returns
// the status and the first bytes of the body, and the freshness check decides
// what they mean.
package httpstamp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
)

// MaxBody is the most body bytes Fetch returns.
const MaxBody = 4 * 1024

// Client fetches stamps through an injectable transport.
type Client struct {
	http *http.Client
}

// New returns a Client that sends requests through transport (nil selects
// http.DefaultTransport). The client never follows redirects and has no
// timeout of its own: the caller bounds each Fetch with its context.
func New(transport http.RoundTripper) *Client {
	return &Client{http: &http.Client{
		Transport:     transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

// Fetch does a GET of url, sending headerName: headerValue when headerName is
// not empty. It returns the response status and at most MaxBody bytes of the
// body, whatever the status; a redirect is returned as it is, not followed.
func (c *Client) Fetch(ctx context.Context, url, headerName, headerValue string) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return 0, nil, fmt.Errorf("build stamp request: %w", withoutURL(err))
	}

	if headerName != "" {
		req.Header.Set(headerName, headerValue)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("fetch stamp: %w", withoutURL(err))
	}

	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxBody))
	if err != nil {
		return 0, nil, fmt.Errorf("read stamp body: %w", err)
	}

	return resp.StatusCode, body, nil
}

// withoutURL drops the url a *url.Error repeats (its query may hold a secret)
// and keeps only the cause.
func withoutURL(err error) error {
	if ue, ok := errors.AsType[*neturl.Error](err); ok {
		return ue.Err
	}

	return err
}
