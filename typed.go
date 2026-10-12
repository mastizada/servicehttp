package servicehttp

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

const (
	maxErrorBody    = 64 << 10 // bytes of an error response kept in Error.Body
	maxErrorMessage = 256      // bytes of Error.Body shown by Error.Error
)

// Error is returned by the typed helpers for responses with status >= 400.
// Retrieve it with errors.AsType[*servicehttp.Error](err).
type Error struct {
	Method     string
	URL        string // without credentials, query or fragment
	StatusCode int
	Header     http.Header // response headers, e.g. Retry-After
	Body       []byte      // raw response body, up to 64 KiB

	// Fields holds the top-level keys when Body is a JSON object, else nil.
	Fields map[string]json.RawMessage
	// Message and Code are extracted automatically from Fields (or from a
	// nested "error" object): Message from "message", "error_description",
	// "error", "detail" or "title"; Code from "code" or "error_code". Empty
	// when absent.
	Message, Code string
}

func newError(method, url string, resp *http.Response, body []byte) *Error {
	e := &Error{Method: method, URL: url, StatusCode: resp.StatusCode, Header: resp.Header, Body: body}
	if json.Unmarshal(body, &e.Fields) != nil {
		e.Fields = nil
		return e
	}
	e.Message, e.Code = extract(e.Fields)
	if nested := e.Fields["error"]; e.Message == "" || e.Code == "" {
		var inner map[string]json.RawMessage
		if json.Unmarshal(nested, &inner) == nil {
			m, c := extract(inner)
			e.Message, e.Code = cmp.Or(e.Message, m), cmp.Or(e.Code, c)
		}
	}
	return e
}

// extract returns the first string value among the message keys and code keys.
func extract(f map[string]json.RawMessage) (msg, code string) {
	for _, k := range []string{"message", "error_description", "error", "detail", "title"} {
		if msg = scalar(f[k]); msg != "" {
			break
		}
	}
	for _, k := range []string{"code", "error_code"} {
		if code = scalar(f[k]); code != "" {
			break
		}
	}
	return msg, code
}

// scalar renders a JSON string or number as text; anything else yields "".
func scalar(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var n json.Number
	if json.Unmarshal(raw, &n) == nil {
		return n.String()
	}
	return ""
}

// Decode unmarshals the raw error body into v, for service-specific error schemas.
func (e *Error) Decode(v any) error { return json.Unmarshal(e.Body, v) }

func (e *Error) Error() string {
	msg := fmt.Sprintf("servicehttp: %s %s: %d %s", e.Method, e.URL, e.StatusCode, http.StatusText(e.StatusCode))
	switch {
	case e.Message != "":
		msg += fmt.Sprintf(": %q", e.Message[:min(len(e.Message), maxErrorMessage)])
	case len(e.Body) > 0:
		msg += fmt.Sprintf(": %q", e.Body[:min(len(e.Body), maxErrorMessage)])
	}
	return msg
}

// Get sends a GET request and decodes the JSON response into T. A nil client
// means [Global]. Non-empty opts derive a client via [Client.With] for this
// call only; derive once and reuse it on hot paths.
func Get[T any](ctx context.Context, c *Client, url string, opts ...Option) (T, error) {
	return Do[T](ctx, c, http.MethodGet, url, nil, opts...)
}

// Post sends body as JSON in a POST request and decodes the JSON response into T.
func Post[T any](ctx context.Context, c *Client, url string, body any, opts ...Option) (T, error) {
	return Do[T](ctx, c, http.MethodPost, url, body, opts...)
}

// GetRaw sends a GET request and returns the undecoded response body, for
// non-JSON or polymorphic payloads. Status >= 400 yields a *[Error]. For JSON
// whose shape is decided later, Get[json.RawMessage] works too.
func GetRaw(ctx context.Context, c *Client, url string, opts ...Option) ([]byte, error) {
	resp, err := roundTrip(ctx, c, http.MethodGet, url, nil, opts) //nolint:bodyclose // closed by drainClose
	if err != nil {
		return nil, err
	}
	defer drainClose(resp.Body)
	return io.ReadAll(resp.Body)
}

// Response is a decoded response together with its status and headers.
type Response[T any] struct {
	Data       T
	StatusCode int
	Header     http.Header
}

// Do sends a request with an optional JSON-encoded body (nil for none) and
// decodes the JSON response into T. Responses with status >= 400 yield a
// *[Error]; a 204 response yields the zero T. Use [DoResponse] when you also
// need the response headers.
func Do[T any](ctx context.Context, c *Client, method, url string, body any, opts ...Option) (T, error) {
	r, err := DoResponse[T](ctx, c, method, url, body, opts...)
	if err != nil {
		var zero T
		return zero, err
	}
	return r.Data, nil
}

// GetResponse is [Get] that also returns the status code and headers.
func GetResponse[T any](ctx context.Context, c *Client, url string, opts ...Option) (*Response[T], error) {
	return DoResponse[T](ctx, c, http.MethodGet, url, nil, opts...)
}

// DoResponse is [Do] that also returns the status code and headers (for
// example Retry-After, ETag or pagination links). Errors carry the headers in
// [Error.Header].
func DoResponse[T any](ctx context.Context, c *Client, method, url string, body any, opts ...Option) (*Response[T], error) {
	resp, err := roundTrip(ctx, c, method, url, body, opts) //nolint:bodyclose // closed by drainClose
	if err != nil {
		return nil, err
	}
	defer drainClose(resp.Body)
	out := &Response[T]{StatusCode: resp.StatusCode, Header: resp.Header}
	if resp.StatusCode == http.StatusNoContent {
		return out, nil
	}
	if err := json.NewDecoder(resp.Body).Decode(&out.Data); err != nil {
		return nil, fmt.Errorf("servicehttp: decode %s %s response: %w", method, redactedURL(resp.Request.URL), err)
	}
	return out, nil
}

// roundTrip sends the request and converts status >= 400 into *Error. On
// success the caller must drainClose resp.Body.
func roundTrip(ctx context.Context, c *Client, method, url string, body any, opts []Option) (*http.Response, error) {
	if c == nil {
		c = Global()
	}
	if len(opts) > 0 {
		c = c.With(opts...)
	}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("servicehttp: encode request body: %w", err)
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if rd != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.Request == nil { // custom transports may not set it
		resp.Request = req
	}
	if resp.StatusCode >= 400 {
		defer drainClose(resp.Body)
		b, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
		return nil, newError(method, redactedURL(req.URL), resp, b)
	}
	return resp, nil
}
