package servicehttp_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/mastizada/servicehttp"
)

type account struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

var fastBackoff = servicehttp.WithBackoff(time.Millisecond, 4*time.Millisecond)

// flaky fails the first `failures` requests with 503, then answers with an account.
func flaky(t *testing.T, failures int32) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) <= failures {
			http.Error(w, "try later", http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte(`{"id":1,"name":"ok"}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func TestGetTyped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept") != "application/json" || r.Header.Get("X-Caller") != "test" {
			http.Error(w, "bad headers", http.StatusBadRequest)
			return
		}
		w.Write([]byte(`{"id":42,"name":"alice"}`))
	}))
	defer srv.Close()

	c := servicehttp.New(servicehttp.WithHeader("X-Caller", "test"))
	got, err := servicehttp.Get[account](t.Context(), c, srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if want := (account{42, "alice"}); got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestPostTypedEncodesBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Content-Type") != "application/json" {
			http.Error(w, "bad content type", http.StatusBadRequest)
			return
		}
		io.Copy(w, r.Body) // echo
	}))
	defer srv.Close()

	got, err := servicehttp.Post[account](t.Context(), servicehttp.New(), srv.URL, account{7, "bob"})
	if err != nil {
		t.Fatal(err)
	}
	if want := (account{7, "bob"}); got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestErrorStatusExtractsBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no such account", http.StatusNotFound)
	}))
	defer srv.Close()

	_, err := servicehttp.Get[account](t.Context(), servicehttp.New(), srv.URL+"/a?token=secret")
	apiErr, ok := errors.AsType[*servicehttp.Error](err)
	if !ok {
		t.Fatalf("want *servicehttp.Error, got %v", err)
	}
	if apiErr.StatusCode != 404 || string(apiErr.Body) != "no such account\n" {
		t.Fatalf("unexpected error: %+v", apiErr)
	}
	if got := apiErr.Error(); strings.Contains(got, "secret") {
		t.Fatalf("query leaked into error message: %s", got)
	}
}

func TestWithSharesTransportAndOverrides(t *testing.T) {
	srv, hits := flaky(t, 100) // always 503
	parent := servicehttp.New()
	child := parent.With(servicehttp.WithMaxRetries(2), fastBackoff, servicehttp.WithTimeout(time.Second))

	if child.Transport() != parent.Transport() {
		t.Fatal("derived client must share the parent's transport")
	}

	if _, err := servicehttp.Get[account](t.Context(), parent, srv.URL); err == nil {
		t.Fatal("want error")
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("parent must not retry: %d hits", got)
	}

	hits.Store(0)
	if _, err := servicehttp.Get[account](t.Context(), child, srv.URL); err == nil {
		t.Fatal("want error")
	}
	if got := hits.Load(); got != 3 {
		t.Fatalf("child: want 1+2 attempts, got %d", got)
	}
}

func TestRetryOn503ThenSuccess(t *testing.T) {
	srv, hits := flaky(t, 2)
	c := servicehttp.New(servicehttp.WithMaxRetries(3), fastBackoff)

	got, err := servicehttp.Get[account](t.Context(), c, srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "ok" || hits.Load() != 3 {
		t.Fatalf("got %+v after %d hits", got, hits.Load())
	}
}

func TestCustomRetryEvaluator(t *testing.T) {
	tests := []struct {
		name     string
		eval     servicehttp.RetryEvaluator
		wantHits int32
	}{
		{"status does not match", servicehttp.RetryOnStatus(429), 1},
		{"5xx", servicehttp.RetryOn5xx(), 3},
		{"custom func inspects response", func(resp *http.Response, err error) bool {
			return resp != nil && resp.StatusCode == http.StatusServiceUnavailable
		}, 3},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv, hits := flaky(t, 100)
			c := servicehttp.New(servicehttp.WithMaxRetries(2), fastBackoff, servicehttp.WithRetryEvaluator(tc.eval))
			_, err := servicehttp.Get[account](t.Context(), c, srv.URL)
			if apiErr, ok := errors.AsType[*servicehttp.Error](err); !ok || apiErr.StatusCode != 503 {
				t.Fatalf("want final 503 *Error, got %v", err)
			}
			if hits.Load() != tc.wantHits {
				t.Fatalf("want %d hits, got %d", tc.wantHits, hits.Load())
			}
		})
	}
}

func TestRetryReplaysBody(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		n := len(bodies)
		mu.Unlock()
		if n < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Write(b)
	}))
	defer srv.Close()

	c := servicehttp.New(servicehttp.WithMaxRetries(3), fastBackoff)
	if _, err := servicehttp.Post[account](t.Context(), c, srv.URL, account{1, "x"}); err != nil {
		t.Fatal(err)
	}
	if len(bodies) != 3 || bodies[0] != bodies[2] || bodies[2] == "" {
		t.Fatalf("body not replayed on retry: %q", bodies)
	}
}

func TestRetryStopsWhenContextCanceled(t *testing.T) {
	srv, hits := flaky(t, 100)
	c := servicehttp.New(servicehttp.WithMaxRetries(50), servicehttp.WithBackoff(time.Second, time.Second))
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()

	if _, err := servicehttp.Get[account](ctx, c, srv.URL); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want deadline exceeded, got %v", err)
	}
	if hits.Load() > 2 {
		t.Fatalf("kept retrying after cancellation: %d hits", hits.Load())
	}
}

func TestGlobalClientThreadSafety(t *testing.T) {
	servicehttp.SetGlobalClient(nil)
	t.Cleanup(func() { servicehttp.SetGlobalClient(nil) })
	srv, _ := flaky(t, 0)

	var wg sync.WaitGroup
	for i := range 32 {
		wg.Go(func() {
			if i%4 == 0 {
				servicehttp.SetGlobalClient(servicehttp.New(servicehttp.WithTimeout(time.Second)))
			}
			if servicehttp.Global() == nil {
				t.Error("Global returned nil")
			}
			if _, err := servicehttp.Get[account](t.Context(), nil, srv.URL); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()

	custom := servicehttp.New()
	servicehttp.SetGlobalClient(custom)
	if servicehttp.Global() != custom {
		t.Fatal("SetGlobalClient did not take effect")
	}
}

// Not parallel: it swaps the global OpenTelemetry providers.
func TestTelemetryFlagsAreIndependent(t *testing.T) {
	prevProp := otel.GetTextMapPropagator()
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() { otel.SetTextMapPropagator(prevProp) })

	tests := []struct {
		name             string
		tracing, metrics bool
	}{
		{"neither", false, false},
		{"tracing only", true, false},
		{"metrics only", false, true},
		{"both", true, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			spans := tracetest.NewSpanRecorder()
			otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spans)))
			reader := sdkmetric.NewManualReader()
			otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))

			var traceparent atomic.Value
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				traceparent.Store(r.Header.Get("Traceparent"))
				w.Write([]byte(`{}`))
			}))
			defer srv.Close()

			c := servicehttp.New(servicehttp.WithTracing(tc.tracing), servicehttp.WithMetrics(tc.metrics))
			// Start a parent span so propagation has a valid context to inject.
			ctx, parent := otel.Tracer("test").Start(t.Context(), "parent")
			if _, err := servicehttp.Get[account](ctx, c, srv.URL); err != nil {
				t.Fatal(err)
			}
			parent.End()

			var rm metricdata.ResourceMetrics
			if err := reader.Collect(t.Context(), &rm); err != nil {
				t.Fatal(err)
			}
			gotMetrics := 0
			for _, sm := range rm.ScopeMetrics {
				gotMetrics += len(sm.Metrics)
			}
			// "parent" is always recorded; a client span is the extra one.
			gotClientSpans := len(spans.Ended()) - 1
			gotHeader := traceparent.Load().(string) != ""

			if (gotClientSpans == 1) != tc.tracing || gotHeader != tc.tracing {
				t.Errorf("tracing=%v: client spans=%d, traceparent sent=%v", tc.tracing, gotClientSpans, gotHeader)
			}
			if (gotMetrics > 0) != tc.metrics {
				t.Errorf("metrics=%v: got %d metrics", tc.metrics, gotMetrics)
			}
		})
	}
}

func TestErrorExtractsJSONFields(t *testing.T) {
	tests := []struct {
		name, body, wantMsg, wantCode string
	}{
		{"flat", `{"message":"bad id","code":"E42"}`, "bad id", "E42"},
		{"numeric code", `{"error":"nope","error_code":1007}`, "nope", "1007"},
		{"nested", `{"error":{"message":"deep","code":"X"}}`, "deep", "X"},
		{"not json", `upstream exploded`, "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusUnprocessableEntity)
				io.WriteString(w, tc.body)
			}))
			defer srv.Close()

			_, err := servicehttp.Get[account](t.Context(), servicehttp.New(), srv.URL)
			e, ok := errors.AsType[*servicehttp.Error](err)
			if !ok {
				t.Fatalf("want *Error, got %v", err)
			}
			if e.Message != tc.wantMsg || e.Code != tc.wantCode || string(e.Body) != tc.body {
				t.Fatalf("got msg=%q code=%q body=%q", e.Message, e.Code, e.Body)
			}
			if (e.Fields != nil) != (tc.name != "not json") {
				t.Fatalf("Fields = %v", e.Fields)
			}
		})
	}
}

func TestRawAndPolymorphicResponses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"type":"card","last4":"1111"}`)
	}))
	defer srv.Close()
	c := servicehttp.New()

	raw, err := servicehttp.GetRaw(t.Context(), c, srv.URL)
	if err != nil || string(raw) != `{"type":"card","last4":"1111"}` {
		t.Fatalf("GetRaw = %q, %v", raw, err)
	}
	msg, err := servicehttp.Get[json.RawMessage](t.Context(), c, srv.URL)
	if err != nil || string(msg) != string(raw) {
		t.Fatalf("Get[RawMessage] = %q, %v", msg, err)
	}
}

func TestNamedGlobalClients(t *testing.T) {
	servicehttp.SetGlobalClient(nil)
	t.Cleanup(func() {
		servicehttp.SetGlobalClient(nil)
		servicehttp.SetGlobalClient(nil, "payments")
	})
	payments := servicehttp.New(servicehttp.WithMaxConnsPerHost(4))
	servicehttp.SetGlobalClient(payments, "payments")

	if servicehttp.Global("payments") != payments {
		t.Fatal("named client not returned")
	}
	def := servicehttp.Global()
	if def == payments || servicehttp.Global("unknown") != def {
		t.Fatal("unnamed and unknown names must resolve to the default client")
	}
	if payments.Transport() == def.Transport() {
		t.Fatal("named clients built with New must not share a pool")
	}
	servicehttp.SetGlobalClient(nil, "payments")
	if servicehttp.Global("payments") != def {
		t.Fatal("named client not removed")
	}
}

func TestResponseHeadersAvailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "7")
		if r.URL.Path == "/limited" {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Write([]byte(`{"id":1}`))
	}))
	defer srv.Close()
	c := servicehttp.New()

	r, err := servicehttp.GetResponse[account](t.Context(), c, srv.URL)
	if err != nil || r.Header.Get("Retry-After") != "7" || r.Data.ID != 1 || r.StatusCode != 200 {
		t.Fatalf("GetResponse = %+v, %v", r, err)
	}
	_, err = servicehttp.Get[account](t.Context(), c, srv.URL+"/limited")
	if e, ok := errors.AsType[*servicehttp.Error](err); !ok || e.Header.Get("Retry-After") != "7" {
		t.Fatalf("Error.Header missing Retry-After: %v", err)
	}

	// Evaluators and plain responses see headers too.
	resp, err := c.Get(t.Context(), srv.URL+"/limited")
	if err != nil || resp.Header.Get("Retry-After") != "7" {
		t.Fatalf("raw response headers: %v", err)
	}
	resp.Body.Close()
}

type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestVerbHelpers(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		io.WriteString(w, r.Method+"|"+r.Header.Get("Content-Type")+"|"+string(b))
	}))
	defer srv.Close()
	c := servicehttp.New()
	ctx := t.Context()

	calls := map[string]func() (*http.Response, error){
		"GET||":              func() (*http.Response, error) { return c.Get(ctx, srv.URL) },
		"DELETE||":           func() (*http.Response, error) { return c.Delete(ctx, srv.URL) },
		"POST|text/plain|a":  func() (*http.Response, error) { return c.Post(ctx, srv.URL, "text/plain", strings.NewReader("a")) },
		"PUT|text/plain|b":   func() (*http.Response, error) { return c.Put(ctx, srv.URL, "text/plain", strings.NewReader("b")) },
		"PATCH|text/plain|c": func() (*http.Response, error) { return c.Patch(ctx, srv.URL, "text/plain", strings.NewReader("c")) },
	}
	for want, call := range calls {
		resp, err := call()
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if string(b) != want {
			t.Errorf("want %q, got %q", want, b)
		}
	}
	if _, err := c.Get(ctx, "://bad"); err == nil {
		t.Fatal("want invalid URL error")
	}
}

func TestTransportOptionsAndNilEvaluator(t *testing.T) {
	c := servicehttp.New(servicehttp.WithMaxIdleConnsPerHost(5), servicehttp.WithMaxConnsPerHost(9))
	if tr := c.Transport().(*http.Transport); tr.MaxIdleConnsPerHost != 5 || tr.MaxConnsPerHost != 9 {
		t.Fatalf("pool options not applied: %+v", tr)
	}

	custom := rtFunc(func(r *http.Request) (*http.Response, error) { return nil, errors.New("down") })
	if servicehttp.New(servicehttp.WithTransport(custom)).Transport() == nil {
		t.Fatal("custom transport not used")
	}

	srv, hits := flaky(t, 100)
	c = servicehttp.New(servicehttp.WithMaxRetries(1), fastBackoff,
		servicehttp.WithRetryEvaluator(servicehttp.RetryOnStatus(429)),
		servicehttp.WithRetryEvaluator(nil)) // back to default: retries 503
	servicehttp.Get[account](t.Context(), c, srv.URL)
	if hits.Load() != 2 {
		t.Fatalf("default evaluator should retry 503: %d hits", hits.Load())
	}
}

func TestRetryTransportErrors(t *testing.T) {
	var hits int
	flakyRT := rtFunc(func(r *http.Request) (*http.Response, error) {
		if hits++; hits < 3 {
			return nil, errors.New("connection reset")
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"id":9}`))}, nil
	})
	c := servicehttp.New(servicehttp.WithTransport(flakyRT), servicehttp.WithMaxRetries(3), fastBackoff)
	got, err := servicehttp.Get[account](t.Context(), c, "http://svc.invalid/a")
	if err != nil || got.ID != 9 || hits != 3 {
		t.Fatalf("got %+v, %v after %d hits", got, err, hits)
	}
}

func TestNonReplayableBodyIsNotRetried(t *testing.T) {
	srv, hits := flaky(t, 100)
	c := servicehttp.New(servicehttp.WithMaxRetries(3), fastBackoff)

	for name, mutate := range map[string]func(*http.Request){
		"no GetBody":    func(r *http.Request) { r.GetBody = nil },
		"GetBody fails": func(r *http.Request) { r.GetBody = func() (io.ReadCloser, error) { return nil, errors.New("boom") } },
	} {
		t.Run(name, func(t *testing.T) {
			hits.Store(0)
			req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL, strings.NewReader("payload"))
			mutate(req)
			resp, err := c.Do(req)
			if err != nil || resp.StatusCode != 503 || hits.Load() != 1 {
				t.Fatalf("want single 503, got resp=%v err=%v hits=%d", resp, err, hits.Load())
			}
			resp.Body.Close()
		})
	}
}

func TestErrorStringAndDecode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/empty":
			w.WriteHeader(http.StatusBadGateway)
		case "/text":
			http.Error(w, "plain failure", 500)
		default:
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"message":"m","extra":5}`)
		}
	}))
	defer srv.Close()
	c := servicehttp.New()

	for path, want := range map[string]string{"/empty": "502 Bad Gateway", "/text": `"plain failure\n"`, "/json": `"m"`} {
		_, err := servicehttp.Get[account](t.Context(), c, srv.URL+path)
		if err == nil || !strings.HasSuffix(err.Error(), want) {
			t.Errorf("%s: error %q should end with %q", path, err, want)
		}
	}
	_, err := servicehttp.Get[account](t.Context(), c, srv.URL+"/json")
	e, _ := errors.AsType[*servicehttp.Error](err)
	var out struct{ Extra int }
	if err := e.Decode(&out); err != nil || out.Extra != 5 {
		t.Fatalf("Decode = %+v, %v", out, err)
	}
}

func TestTypedEdgeCases(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/204":
			w.WriteHeader(http.StatusNoContent)
		case "/500":
			w.WriteHeader(500)
		default:
			io.WriteString(w, "not json")
		}
	}))
	defer srv.Close()
	c := servicehttp.New()
	ctx := t.Context()

	if v, err := servicehttp.Do[account](ctx, c, http.MethodDelete, srv.URL+"/204", nil); err != nil || v != (account{}) {
		t.Fatalf("204: %+v, %v", v, err)
	}
	if r, err := servicehttp.GetResponse[account](ctx, c, srv.URL+"/204"); err != nil || r.StatusCode != 204 {
		t.Fatalf("204 response: %+v, %v", r, err)
	}
	if _, err := servicehttp.Get[account](ctx, c, srv.URL+"/bad"); err == nil || !strings.Contains(err.Error(), "decode") {
		t.Fatalf("want decode error, got %v", err)
	}
	if _, err := servicehttp.GetRaw(ctx, c, srv.URL+"/500"); !isAPIError(err) {
		t.Fatalf("GetRaw 500: %v", err)
	}
	if _, err := servicehttp.GetRaw(ctx, c, srv.URL+"/bad", servicehttp.WithTimeout(time.Second)); err != nil {
		t.Fatalf("GetRaw with opts: %v", err)
	}
	if _, err := servicehttp.Post[account](ctx, c, srv.URL, make(chan int)); err == nil || !strings.Contains(err.Error(), "encode") {
		t.Fatalf("want encode error, got %v", err)
	}
	if _, err := servicehttp.Get[account](ctx, c, "://bad"); err == nil {
		t.Fatal("want invalid URL error")
	}
	srv.Close()
	if _, err := servicehttp.Get[account](ctx, c, srv.URL); err == nil {
		t.Fatal("want connection error")
	}
}

func isAPIError(err error) bool { _, ok := errors.AsType[*servicehttp.Error](err); return ok }

type badMeter struct{ noop.Meter }

func (badMeter) Float64Histogram(string, ...metric.Float64HistogramOption) (metric.Float64Histogram, error) {
	return noop.Float64Histogram{}, errors.New("instrument rejected")
}

type badMeterProvider struct{ noop.MeterProvider }

func (badMeterProvider) Meter(string, ...metric.MeterOption) metric.Meter { return badMeter{} }

func TestTelemetryEdgeCases(t *testing.T) {
	prev := otel.GetMeterProvider()
	t.Cleanup(func() { otel.SetMeterProvider(prev) })

	t.Run("derived client can turn tracing on", func(t *testing.T) {
		spans := tracetest.NewSpanRecorder()
		otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spans)))
		srv, _ := flaky(t, 0)
		parent := servicehttp.New()
		if _, err := servicehttp.Get[account](t.Context(), parent.With(servicehttp.WithTracing(true)), srv.URL); err != nil {
			t.Fatal(err)
		}
		if _, err := servicehttp.Get[account](t.Context(), parent, srv.URL); err != nil {
			t.Fatal(err)
		}
		if n := len(spans.Ended()); n != 1 {
			t.Fatalf("want 1 span (derived only), got %d", n)
		}
	})

	t.Run("transport errors and error statuses are recorded", func(t *testing.T) {
		spans := tracetest.NewSpanRecorder()
		otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spans)))
		reader := sdkmetric.NewManualReader()
		otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))

		down := rtFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("down") })
		c := servicehttp.New(servicehttp.WithTransport(down), servicehttp.WithTracing(true), servicehttp.WithMetrics(true))
		if _, err := c.Get(t.Context(), "http://svc.invalid/"); err == nil {
			t.Fatal("want error")
		}
		srv, _ := flaky(t, 100)
		c = servicehttp.New(servicehttp.WithTracing(true), servicehttp.WithMetrics(true))
		servicehttp.Get[account](t.Context(), c, srv.URL)

		ended := spans.Ended()
		if len(ended) != 2 || ended[0].Status().Code != codes.Error || ended[1].Status().Code != codes.Error {
			t.Fatalf("want 2 error spans, got %d", len(ended))
		}
		var rm metricdata.ResourceMetrics
		if err := reader.Collect(t.Context(), &rm); err != nil || len(rm.ScopeMetrics) == 0 {
			t.Fatalf("no metrics: %v", err)
		}
	})

	t.Run("instrument creation failure still yields a working client", func(t *testing.T) {
		otel.SetMeterProvider(badMeterProvider{})
		srv, _ := flaky(t, 0)
		c := servicehttp.New(servicehttp.WithMetrics(true))
		if _, err := servicehttp.Get[account](t.Context(), c, srv.URL); err != nil {
			t.Fatal(err)
		}
	})
}
