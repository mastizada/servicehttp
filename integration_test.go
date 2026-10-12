//go:build integration

package servicehttp_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/mastizada/servicehttp"
)

// These tests call the public Star Wars API and need internet access.
// Run them with `make integration-test`.

const swapi = "https://swapi.dev/api"

type person struct {
	Name      string   `json:"name"`
	BirthYear string   `json:"birth_year"`
	Films     []string `json:"films"`
}

func swapiClient() *servicehttp.Client {
	return servicehttp.New(servicehttp.WithTimeout(15*time.Second), servicehttp.WithMaxRetries(2))
}

func TestIntegrationTypedGet(t *testing.T) {
	p, err := servicehttp.Get[person](t.Context(), swapiClient(), swapi+"/people/1/")
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "Luke Skywalker" || p.BirthYear != "19BBY" || len(p.Films) == 0 {
		t.Fatalf("unexpected person: %+v", p)
	}
}

func TestIntegrationResponseHeaders(t *testing.T) {
	r, err := servicehttp.GetResponse[person](t.Context(), swapiClient(), swapi+"/people/1/")
	if err != nil {
		t.Fatal(err)
	}
	if r.StatusCode != http.StatusOK || !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		t.Fatalf("status=%d headers=%v", r.StatusCode, r.Header)
	}
}

func TestIntegrationNotFoundError(t *testing.T) {
	_, err := servicehttp.Get[person](t.Context(), swapiClient(), swapi+"/people/9999/")
	e, ok := errors.AsType[*servicehttp.Error](err)
	if !ok {
		t.Fatalf("want *servicehttp.Error, got %v", err)
	}
	if e.StatusCode != http.StatusNotFound || e.Message != "Not found." {
		t.Fatalf("status=%d message=%q body=%s", e.StatusCode, e.Message, e.Body)
	}
}

func TestIntegrationRawPayloads(t *testing.T) {
	c := swapiClient()
	raw, err := servicehttp.GetRaw(t.Context(), c, swapi+"/planets/1/")
	if err != nil || !json.Valid(raw) {
		t.Fatalf("GetRaw: valid=%v err=%v", json.Valid(raw), err)
	}
	msg, err := servicehttp.Get[json.RawMessage](t.Context(), c, swapi+"/planets/1/")
	if err != nil {
		t.Fatal(err)
	}
	var planet struct{ Name string }
	if err := json.Unmarshal(msg, &planet); err != nil || planet.Name != "Tatooine" {
		t.Fatalf("planet=%+v err=%v", planet, err)
	}
}

func TestIntegrationDerivedClientsShareConnections(t *testing.T) {
	parent := swapiClient()
	child := parent.With(servicehttp.WithTimeout(10*time.Second), servicehttp.WithMaxRetries(0))
	if parent.Transport() != child.Transport() {
		t.Fatal("derived client must share the transport")
	}

	var wg sync.WaitGroup
	for i := range 6 {
		wg.Go(func() {
			c := parent
			if i%2 == 1 {
				c = child
			}
			if _, err := servicehttp.Get[person](t.Context(), c, swapi+"/people/1/"); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
}

func TestIntegrationNamedGlobalClient(t *testing.T) {
	servicehttp.SetGlobalClient(swapiClient(), "swapi")
	t.Cleanup(func() { servicehttp.SetGlobalClient(nil, "swapi") })

	p, err := servicehttp.Get[person](t.Context(), servicehttp.Global("swapi"), swapi+"/people/2/")
	if err != nil || p.Name != "C-3PO" {
		t.Fatalf("person=%+v err=%v", p, err)
	}
}

func TestIntegrationTelemetry(t *testing.T) {
	spans := tracetest.NewSpanRecorder()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spans)))
	reader := sdkmetric.NewManualReader()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))

	c := swapiClient().With(servicehttp.WithTracing(true), servicehttp.WithMetrics(true))
	if _, err := servicehttp.Get[person](t.Context(), c, swapi+"/people/1/"); err != nil {
		t.Fatal(err)
	}
	if n := len(spans.Ended()); n != 1 {
		t.Fatalf("want 1 client span, got %d", n)
	}
}
