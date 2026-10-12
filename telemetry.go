package servicehttp

import (
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

const instrumentationScope = "github.com/mastizada/servicehttp"

// telemetryTransport instruments each attempt (so retries show up as separate
// spans and samples). A nil tracer or histogram means that signal is off.
type telemetryTransport struct {
	base     http.RoundTripper
	tracer   trace.Tracer
	duration metric.Float64Histogram
}

// wrapTelemetry returns base itself when both signals are disabled, so a
// disabled client carries no telemetry code on its request path.
func wrapTelemetry(base http.RoundTripper, tracing, metrics bool) http.RoundTripper {
	if !tracing && !metrics {
		return base
	}
	t := &telemetryTransport{base: base}
	if tracing {
		t.tracer = otel.GetTracerProvider().Tracer(instrumentationScope)
	}
	if metrics {
		h, err := otel.GetMeterProvider().Meter(instrumentationScope).Float64Histogram(
			"http.client.request.duration",
			metric.WithUnit("s"),
			metric.WithDescription("Duration of outbound HTTP request attempts."),
		)
		if err != nil {
			otel.Handle(err) // h is a usable no-op instrument
		}
		t.duration = h
	}
	return t
}

// RoundTrip implements [http.RoundTripper]. Spans end when response headers
// arrive, not when the body has been read.
func (t *telemetryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	start := time.Now()
	ctx := req.Context()
	method := attribute.String("http.request.method", req.Method)
	host := attribute.String("server.address", req.URL.Hostname())

	var span trace.Span
	if t.tracer != nil {
		ctx, span = t.tracer.Start(ctx, req.Method,
			trace.WithSpanKind(trace.SpanKindClient),
			trace.WithAttributes(method, host, attribute.String("url.full", redactedURL(req.URL))))
		req = req.Clone(ctx) // RoundTrippers must not mutate the caller's request
		otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(req.Header))
	}

	resp, err := t.base.RoundTrip(req)

	var errType string
	switch {
	case err != nil:
		errType = fmt.Sprintf("%T", err)
	case resp.StatusCode >= 400:
		errType = strconv.Itoa(resp.StatusCode)
	}

	if span != nil {
		if resp != nil {
			span.SetAttributes(attribute.Int("http.response.status_code", resp.StatusCode))
		}
		if err != nil {
			span.RecordError(err)
		}
		if errType != "" {
			span.SetAttributes(attribute.String("error.type", errType))
			span.SetStatus(codes.Error, "")
		}
		span.End()
	}
	if t.duration != nil {
		attrs := []attribute.KeyValue{method, host}
		if resp != nil {
			attrs = append(attrs, attribute.Int("http.response.status_code", resp.StatusCode))
		}
		if errType != "" {
			attrs = append(attrs, attribute.String("error.type", errType))
		}
		t.duration.Record(req.Context(), time.Since(start).Seconds(), metric.WithAttributes(attrs...))
	}
	return resp, err
}

// redactedURL drops credentials, query and fragment, which commonly carry
// secrets, before a URL is put into a span or an error message.
func redactedURL(u *url.URL) string {
	r := *u
	r.User, r.RawQuery, r.Fragment, r.RawFragment = nil, "", "", ""
	return r.String()
}
