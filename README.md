# servicehttp

A small, production-minded HTTP client for **service-to-service calls** in Go. It wraps `net/http` instead of hiding it, and adds the parts every internal client ends up rewriting: pooling defaults, retries, typed JSON, structured errors and opt-in OpenTelemetry.

Requires Go 1.26+.

```sh
go get github.com/mastizada/servicehttp
```

## Features

- **Thin wrapper** over `http.Client` / `http.Transport`; the transport is always reachable via `client.Transport()`.
- **Tuned defaults**: 5s per-attempt timeout, keep-alives, HTTP/2, 100 idle connections per host (see [defaults](#defaults)).
- **Typed JSON**: `Get[T]`, `Post[T]`, `Do[T]`, plus `GetRaw` and `Get[json.RawMessage]` for polymorphic payloads.
- **Structured errors**: any status >= 400 becomes `*servicehttp.Error` with raw body, headers and auto-extracted `Message` / `Code`.
- **Retries**: exponential backoff with full jitter and pluggable `RetryEvaluator`s.
- **Derived clients** (`.With()`) that share one connection pool but override timeout, retries, headers and telemetry.
- **Global and named clients** for isolating pools per use case.
- **OpenTelemetry**, tracing and metrics independently opt-in, using the global providers. Zero code on the request path when disabled.

## Quick start

```go
type Account struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

acct, err := servicehttp.Get[Account](ctx, nil, "http://accounts/v1/accounts/42") // nil = default global client
if err != nil {
	if apiErr, ok := errors.AsType[*servicehttp.Error](err); ok {
		log.Printf("status=%d code=%s msg=%s", apiErr.StatusCode, apiErr.Code, apiErr.Message)
	}
	return err
}

log.Printf("account: %+v", acct)
// 2026/10/12 09:30:12 account: {ID:42 Name:alice}
```

## Clients

```go
c := servicehttp.New(
	servicehttp.WithTimeout(3*time.Second),
	servicehttp.WithMaxRetries(2),
	servicehttp.WithHeader("User-Agent", "orders/1.4"),
	servicehttp.WithTracing(true),
	servicehttp.WithMetrics(true),
)
```

| Option | Default | Notes |
|---|---|---|
| `WithTimeout(d)` | `5s` | Per attempt, includes reading the body. `0` disables. |
| `WithMaxRetries(n)` | `0` | Retries after the first attempt. Off by default so POSTs are never replayed by accident. |
| `WithRetryEvaluator(e)` | errors + 502/503/504 | `nil` restores the default. |
| `WithBackoff(base, max)` | `50ms`, `1s` | Delay before retry n is random in `[0, min(max, base*2^n)]`. |
| `WithHeader(k, v)` | none | Default header; request headers win. |
| `WithTracing(bool)` / `WithMetrics(bool)` | off | Independent. |
| `WithMaxIdleConnsPerHost(n)` | `100` | `New` only. |
| `WithMaxConnsPerHost(n)` | unlimited | `New` only. |
| `WithTransport(rt)` | built-in | `New` only. Bring your own TLS, proxies, test doubles. |

#### Defaults

Tuned for API microservices calling a handful of upstreams: fail fast, keep many warm connections, never hammer a struggling dependency.

| Setting | Value | Why |
|---|---|---|
| Per-attempt timeout | `5s` | Bounds tail latency so callers fail fast. |
| Dial timeout | `2s` | An unreachable pod should not eat the request budget. |
| TLS handshake timeout | `3s` | Same reasoning. |
| Idle connections per host | `100` | Go's stdlib default of 2 forces reconnects under concurrent load. |
| Idle connection timeout | `30s` | Below the 60s idle timeout common on load balancers, avoiding reuse of connections the peer just closed. |
| Max connections per host | unlimited | Cap it per upstream with `WithMaxConnsPerHost`. |
| Retries | `0` | Retry amplification causes outages and POSTs are not always idempotent; enable per integration. |
| Backoff | `50ms` to `1s`, full jitter | Short enough to fit a few retries inside a typical request deadline. |

The plain methods (`Get`, `Post`, `Put`, `Patch`, `Delete`, `Do`) return `*http.Response` and apply retries, but do not turn 4xx/5xx into errors.

### Derived clients: one pool, many policies

`.With()` returns a copy that **shares the parent's transport and connection pool**:

```go
base := servicehttp.New()
payments := base.With(servicehttp.WithTimeout(2*time.Second), servicehttp.WithMaxRetries(3))
reports := base.With(servicehttp.WithTimeout(30 * time.Second))

payments.Transport() == base.Transport() // true
```

Derive once and reuse. Passing `opts` to `Get[T]` and friends derives a client for that call only, which is convenient but allocates.

### Global and named clients

```go
servicehttp.SetGlobalClient(servicehttp.New(servicehttp.WithTimeout(5*time.Second)))   // default
servicehttp.SetGlobalClient(servicehttp.New(servicehttp.WithMaxConnsPerHost(20)), "payments")

servicehttp.Global()           // default (lazily created if never set)
servicehttp.Global("payments") // named
```

- Build each named client with its **own `New`** to isolate its connection pool. Clients created with `.With()` share their parent's pool.
- An unregistered name falls back to the default client.
- `SetGlobalClient(nil, name)` removes a named client; `SetGlobalClient(nil)` resets the default.
- All of this is safe for concurrent use.

## Typed requests

```go
created, err := servicehttp.Post[Account](ctx, c, url, Account{Name: "alice"}) // body is JSON-encoded
deleted, err := servicehttp.Do[struct{}](ctx, c, http.MethodDelete, url, nil)  // 204 -> zero value
```

Requests send `Accept: application/json`, and `Content-Type: application/json` when there is a body.

### Headers and status

```go
r, err := servicehttp.GetResponse[Account](ctx, c, url)
etag := r.Header.Get("ETag") // also r.StatusCode, r.Data
```

### Polymorphic or non-JSON payloads

```go
raw, err := servicehttp.GetRaw(ctx, c, url)                      // []byte, no decoding
msg, err := servicehttp.Get[json.RawMessage](ctx, c, url)        // decode later by "type"
```

## Errors

Every status >= 400 from the typed helpers returns `*servicehttp.Error`:

| Field | Meaning |
|---|---|
| `StatusCode`, `Method`, `URL` | URL has credentials, query and fragment stripped. |
| `Header` | Response headers, e.g. `Retry-After`. |
| `Body` | Raw body, up to 64 KiB. |
| `Fields` | Top-level keys when the body is a JSON object. |
| `Message`, `Code` | Auto-extracted. Message from `message`, `error_description`, `error`, `detail` or `title`; code from `code` or `error_code`. Nested `{"error": {...}}` is understood. |

`e.Decode(&v)` unmarshals the body into your own error schema.

## Retries

```go
c := servicehttp.New(
	servicehttp.WithMaxRetries(3),
	servicehttp.WithRetryEvaluator(servicehttp.RetryAny(
		servicehttp.RetryOnError(),
		servicehttp.RetryOnStatus(429, 503),
	)),
)

// Fully custom: evaluators see the status and headers (do not read the body).
custom := func(resp *http.Response, err error) bool {
	return resp != nil && resp.Header.Get("Retry-After") != ""
}
```

Built-ins: `RetryOnStatus(codes...)`, `RetryOn5xx()`, `RetryOnError()`, `RetryAny(evals...)`.

- Retries stop when the request context is done.
- Retried bodies must be replayable (`bytes.Reader`, `bytes.Buffer`, `strings.Reader`, and all typed JSON calls). Otherwise the first outcome is returned.
- The last response is returned when retries run out. `Retry-After` is not honored automatically; read it in a custom evaluator if you need it.

## OpenTelemetry

```go
otel.SetTracerProvider(tp)
otel.SetMeterProvider(mp)
otel.SetTextMapPropagator(propagation.TraceContext{}) // required for traceparent injection

c := servicehttp.New(servicehttp.WithTracing(true), servicehttp.WithMetrics(false))
```

- **Tracing**: one client span per attempt (retries are visible), plus W3C context injection. Spans end when headers arrive, not after the body is read.
- **Metrics**: `http.client.request.duration` histogram (seconds) with method, host, status and error type.
- **Opt-in**: with both flags off the telemetry wrapper is not installed. The library never sets global providers or propagators.

## Development

```sh
make fmt               # format
make lint              # golangci-lint
make test              # unit tests with race detector and coverage
make integration-test  # end-to-end tests against https://swapi.dev (needs internet)
```

See [AGENTS.md](AGENTS.md) for contributor and agent notes.
