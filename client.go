// Package servicehttp is a small HTTP client for service-to-service calls.
// It wraps net/http with tuned connection pooling, retries with exponential
// backoff and jitter, typed JSON helpers and opt-in OpenTelemetry.
//
// Derive per-integration clients with [Client.With]; they share the parent's
// transport and therefore its connection pool:
//
//	payments := servicehttp.Global().With(
//		servicehttp.WithTimeout(2*time.Second),
//		servicehttp.WithMaxRetries(3),
//	)
//	acct, err := servicehttp.Get[Account](ctx, payments, "http://payments/v1/accounts/42")
package servicehttp

import (
	"context"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"time"
)

// Client is an HTTP client with retries. It is safe for concurrent use and
// immutable once built; [Client.With] returns a modified copy.
type Client struct {
	cfg  Config
	base http.RoundTripper // shared by all derived clients
	hc   *http.Client      // base wrapped with telemetry as configured
}

// New returns a client with defaults tuned for east-west traffic: a 10s
// per-attempt timeout, keep-alives and 64 idle connections per host.
func New(opts ...Option) *Client {
	cfg := defaultConfig()
	for _, o := range opts {
		o(&cfg)
	}
	base := cfg.Transport
	if base == nil {
		base = &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: (&net.Dialer{
				Timeout:   5 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConnsPerHost:   cfg.MaxIdleConnsPerHost,
			MaxConnsPerHost:       cfg.MaxConnsPerHost,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   5 * time.Second,
			ExpectContinueTimeout: time.Second,
		}
	}
	return newClient(base, wrapTelemetry(base, cfg.Tracing, cfg.Metrics), cfg)
}

func newClient(base, rt http.RoundTripper, cfg Config) *Client {
	return &Client{cfg: cfg, base: base, hc: &http.Client{Transport: rt, Timeout: cfg.Timeout}}
}

// With returns a derived client with opts applied. It shares the parent's
// transport and connection pool; Timeout, retry settings, default headers and
// telemetry flags can differ. The parent is unchanged.
func (c *Client) With(opts ...Option) *Client {
	cfg := c.cfg
	cfg.Header = c.cfg.Header.Clone()
	for _, o := range opts {
		o(&cfg)
	}
	rt := c.hc.Transport
	if cfg.Tracing != c.cfg.Tracing || cfg.Metrics != c.cfg.Metrics {
		rt = wrapTelemetry(c.base, cfg.Tracing, cfg.Metrics)
	}
	return newClient(c.base, rt, cfg)
}

// Transport returns the underlying transport shared by this client and every
// client derived from it, e.g. to call CloseIdleConnections on it.
func (c *Client) Transport() http.RoundTripper { return c.base }

// Do sends req, retrying according to the client's configuration, and returns
// the last response. A retried request needs a replayable body: requests built
// from a bytes.Reader, bytes.Buffer or strings.Reader by http.NewRequest
// qualify; otherwise the first outcome is returned. The caller's req is not
// modified. A 4xx/5xx response is not an error here; see [Do] for that.
func (c *Client) Do(req *http.Request) (*http.Response, error) {
	ctx := req.Context()
	req = req.Clone(ctx)
	for k, v := range c.cfg.Header {
		if _, ok := req.Header[k]; !ok {
			req.Header[k] = v
		}
	}
	for attempt := 0; ; attempt++ {
		resp, err := c.hc.Do(req)
		if attempt >= c.cfg.MaxRetries || ctx.Err() != nil || !c.cfg.RetryEvaluator(resp, err) {
			return resp, err
		}
		body := req.Body
		if body != nil && body != http.NoBody {
			if req.GetBody == nil {
				return resp, err
			}
			var gerr error
			if body, gerr = req.GetBody(); gerr != nil {
				return resp, err
			}
		}
		if resp != nil {
			drainClose(resp.Body)
		}
		t := time.NewTimer(c.backoff(attempt))
		select {
		case <-ctx.Done():
			t.Stop()
			return nil, ctx.Err()
		case <-t.C:
		}
		req.Body = body
	}
}

// backoff returns a full-jitter delay for the given retry number.
func (c *Client) backoff(attempt int) time.Duration {
	d := min(c.cfg.BackoffMax, c.cfg.BackoffBase<<min(attempt, 30))
	return rand.N(d + 1)
}

// Get sends a GET request.
func (c *Client) Get(ctx context.Context, url string) (*http.Response, error) {
	return c.send(ctx, http.MethodGet, url, "", nil)
}

// Delete sends a DELETE request.
func (c *Client) Delete(ctx context.Context, url string) (*http.Response, error) {
	return c.send(ctx, http.MethodDelete, url, "", nil)
}

// Post sends a POST request with the given content type and body.
func (c *Client) Post(ctx context.Context, url, contentType string, body io.Reader) (*http.Response, error) {
	return c.send(ctx, http.MethodPost, url, contentType, body)
}

// Put sends a PUT request with the given content type and body.
func (c *Client) Put(ctx context.Context, url, contentType string, body io.Reader) (*http.Response, error) {
	return c.send(ctx, http.MethodPut, url, contentType, body)
}

// Patch sends a PATCH request with the given content type and body.
func (c *Client) Patch(ctx context.Context, url, contentType string, body io.Reader) (*http.Response, error) {
	return c.send(ctx, http.MethodPatch, url, contentType, body)
}

func (c *Client) send(ctx context.Context, method, url, contentType string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	return c.Do(req)
}

// drainClose reads a bounded amount of the remaining body so the connection
// can be reused, then closes it.
func drainClose(body io.ReadCloser) {
	_, _ = io.Copy(io.Discard, io.LimitReader(body, 4<<10))
	_ = body.Close()
}
