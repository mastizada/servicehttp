# AGENTS.md

Guidance for AI agents and contributors working on `servicehttp`.

## What this is

A small Go (1.26+) HTTP client library for service-to-service calls. Single package, no `main`. Keep it small: no interface with one implementation, no config for values that never change, no speculative features.

## Layout

| File | Responsibility |
|---|---|
| `client.go` | `Client`, `New`, `.With`, retry loop (`Do`), backoff, verb helpers |
| `options.go` | `Config`, functional `Option`s, `RetryEvaluator` + helpers, defaults |
| `global.go` | Default and named global clients (`Global`, `SetGlobalClient`) |
| `typed.go` | Generic helpers (`Get/Post/Do[T]`, `*Response`, `GetRaw`), `Error`, `roundTrip` |
| `telemetry.go` | Opt-in OTel `RoundTripper`, `redactedURL` |
| `client_test.go` | Unit tests (external package `servicehttp_test`) |
| `integration_test.go` | End-to-end tests against swapi.dev (`integration` build tag) |
| `Makefile`, `.golangci.yml` | Dev commands and lint config |

## Commands

```sh
make fmt               # format
make lint              # golangci-lint (config: .golangci.yml), must report 0 issues
make test              # unit tests, -race, coverage (currently 100%; keep it there)
make integration-test  # end-to-end against https://swapi.dev, needs internet
```

Tests always run with `-race`; global client and retry code are concurrency sensitive. Integration tests live in `integration_test.go` behind the `integration` build tag, and their names start with `TestIntegration`.

## Invariants (do not break)

- **`.With()` shares the parent's `base` transport.** Only `Timeout`, retries, backoff, headers and telemetry flags may differ. `Transport`, `MaxIdleConnsPerHost`, `MaxConnsPerHost` are honored by `New` only. A test asserts transport identity.
- **Retries are off by default** (`MaxRetries` 0). The default evaluator retries errors and 502/503/504.
- **Retry loop** clones the request, replays bodies via `GetBody` (no `GetBody` means no retry), drains/closes the previous response, and stops when the request context is done.
- **Telemetry is opt-in and independent.** Disabled flags must add no code to the request path (`wrapTelemetry` returns `base` when both are off). Never call `otel.SetTracerProvider`, `SetMeterProvider` or `SetTextMapPropagator` in library code.
- **Secrets never reach logs or spans:** use `redactedURL` for any URL in errors, spans or messages.
- **Status >= 400 in typed helpers always yields `*Error`** (raw `Body` capped at 64 KiB, `Header` kept, `Message`/`Code` extracted). The untyped `Client` methods do not convert statuses to errors.
- **`Global(name...)`**: unnamed or unregistered names resolve to the default client. The default stays an `atomic.Pointer[Client]`.
- `Client` is immutable after construction; `With` copies `Config` and clones `Header`.

## Conventions

- Go generics cannot be methods, so typed helpers are package-level functions taking `*Client` (nil means `Global()`).
- Every exported identifier has a doc comment. Behavior notes belong in doc comments and `README.md`.
- Tests: table-driven, `httptest.Server`, `t.Context()`, `errors.AsType`, `wg.Go`. Use `fastBackoff` in retry tests so they stay quick. Telemetry tests swap global OTel providers, so they must not run in parallel.
- Keep dependencies to `go.opentelemetry.io/otel` (+ `metric`, `trace`). The OTel SDK is test-only.
- If the module path `github.com/mastizada/servicehttp` changes, update it in `go.mod`, `telemetry.go` (`instrumentationScope`), tests and docs together.

## Adding a feature checklist

1. Does it need to exist? Can an option or an existing helper cover it?
2. Add the option/helper with a doc comment; note whether `.With()` honors it.
3. Add a test next to the closest existing one.
4. Update `README.md` (option table or relevant section) and this file if an invariant changes.
5. Run the commands above.

## Known gaps

- `Retry-After` is not honored automatically (readable via evaluators and `Error.Header`).
- No request-size or response-size limit on successful decodes.
- `Global("typo")` silently returns the default client.
