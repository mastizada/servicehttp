package servicehttp

import (
	"net/http"
	"slices"
	"time"
)

// RetryEvaluator reports whether a finished attempt should be retried. resp is
// nil when err is non-nil. Evaluators must not read or close resp.Body; they may
// inspect the status code and headers. The retry loop itself stops when the
// request context is done or [Config.MaxRetries] is exhausted.
type RetryEvaluator func(resp *http.Response, err error) bool

// RetryOnStatus retries when the response status is one of codes.
func RetryOnStatus(codes ...int) RetryEvaluator {
	return func(resp *http.Response, _ error) bool {
		return resp != nil && slices.Contains(codes, resp.StatusCode)
	}
}

// RetryOn5xx retries on any 5xx response.
func RetryOn5xx() RetryEvaluator {
	return func(resp *http.Response, _ error) bool {
		return resp != nil && resp.StatusCode >= 500
	}
}

// RetryOnError retries when the attempt failed with a transport error.
func RetryOnError() RetryEvaluator {
	return func(_ *http.Response, err error) bool { return err != nil }
}

// RetryAny retries when at least one of evals does.
func RetryAny(evals ...RetryEvaluator) RetryEvaluator {
	return func(resp *http.Response, err error) bool {
		return slices.ContainsFunc(evals, func(e RetryEvaluator) bool { return e(resp, err) })
	}
}

// defaultRetryEvaluator retries transport errors and 502/503/504, the statuses
// that signal a transient upstream failure rather than a bug in the request.
var defaultRetryEvaluator = RetryAny(RetryOnError(), RetryOnStatus(
	http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout))

// Config is the client configuration that [Option] values mutate.
type Config struct {
	// Timeout bounds each attempt, including reading the response body.
	// Zero disables it.
	Timeout time.Duration
	// MaxRetries is the number of retries after the first attempt.
	MaxRetries int
	// RetryEvaluator decides whether an attempt is retried.
	RetryEvaluator RetryEvaluator
	// BackoffBase and BackoffMax bound the exponential backoff. The delay
	// before retry n is uniformly random in [0, min(BackoffMax, BackoffBase*2^n)].
	BackoffBase, BackoffMax time.Duration
	// Header holds default headers; request headers take precedence.
	Header http.Header
	// Tracing and Metrics independently enable OpenTelemetry instrumentation.
	Tracing, Metrics bool

	// Transport and MaxIdleConnsPerHost are only honored by [New]; derived
	// clients always share their parent's transport.
	Transport           http.RoundTripper
	MaxIdleConnsPerHost int
	MaxConnsPerHost     int // 0 means unlimited
}

func defaultConfig() Config {
	return Config{
		Timeout:             10 * time.Second,
		RetryEvaluator:      defaultRetryEvaluator,
		BackoffBase:         100 * time.Millisecond,
		BackoffMax:          2 * time.Second,
		MaxIdleConnsPerHost: 64,
	}
}

// Option configures a [Client] in [New] or [Client.With].
type Option func(*Config)

// WithTimeout sets the per-attempt timeout. Zero disables it.
func WithTimeout(d time.Duration) Option { return func(c *Config) { c.Timeout = d } }

// WithMaxRetries sets the number of retries after the first attempt.
// Retries are off (0) by default; values below zero are treated as zero.
func WithMaxRetries(n int) Option { return func(c *Config) { c.MaxRetries = max(n, 0) } }

// WithRetryEvaluator sets the retry predicate. Nil restores the default, which
// retries transport errors and 502/503/504 responses.
func WithRetryEvaluator(e RetryEvaluator) Option {
	return func(c *Config) {
		if e == nil {
			e = defaultRetryEvaluator
		}
		c.RetryEvaluator = e
	}
}

// WithBackoff sets the exponential backoff base and cap.
func WithBackoff(base, maxDelay time.Duration) Option {
	return func(c *Config) { c.BackoffBase, c.BackoffMax = max(base, 0), max(maxDelay, 0) }
}

// WithHeader sets a default header sent on every request unless the request
// already sets it.
func WithHeader(key, value string) Option {
	return func(c *Config) {
		if c.Header == nil {
			c.Header = http.Header{}
		}
		c.Header.Set(key, value)
	}
}

// WithTracing enables or disables OpenTelemetry tracing and W3C trace-context
// propagation, using otel.GetTracerProvider and otel.GetTextMapPropagator.
// Disabled, no tracing code runs.
func WithTracing(enabled bool) Option { return func(c *Config) { c.Tracing = enabled } }

// WithMetrics enables or disables the http.client.request.duration histogram,
// using otel.GetMeterProvider. Disabled, no metrics code runs.
func WithMetrics(enabled bool) Option { return func(c *Config) { c.Metrics = enabled } }

// WithTransport replaces the default transport. Only honored by [New].
func WithTransport(rt http.RoundTripper) Option { return func(c *Config) { c.Transport = rt } }

// WithMaxIdleConnsPerHost sizes the idle connection pool of the default
// transport. Only honored by [New], and ignored when [WithTransport] is set.
func WithMaxIdleConnsPerHost(n int) Option { return func(c *Config) { c.MaxIdleConnsPerHost = n } }

// WithMaxConnsPerHost caps total (idle + active) connections per host on the
// default transport; 0 is unlimited. Only honored by [New], and ignored when
// [WithTransport] is set.
func WithMaxConnsPerHost(n int) Option { return func(c *Config) { c.MaxConnsPerHost = n } }
