// SPDX-License-Identifier: Apache-2.0

package telemetry_test

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"go.opentelemetry.io/otel"

	"go.riptides.io/waterwheel/internal/telemetry"
)

const (
	endpoint  = "OTEL_EXPORTER_OTLP_ENDPOINT"
	exporter  = "OTEL_METRICS_EXPORTER"
	collector = "http://c:4318"
)

func TestEnabled(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		env  map[string]string
		want bool
	}{
		"nothing set":         {env: nil, want: false},
		"an endpoint":         {env: map[string]string{endpoint: collector}, want: true},
		"a metrics endpoint":  {env: map[string]string{"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT": "http://c:4318/v1/metrics"}, want: true},
		"otlp exporter":       {env: map[string]string{endpoint: collector, exporter: "otlp"}, want: true},
		"exporter none":       {env: map[string]string{endpoint: collector, exporter: "none"}, want: false},
		"another exporter":    {env: map[string]string{endpoint: collector, exporter: "prometheus"}, want: false},
		"SDK disabled":        {env: map[string]string{endpoint: collector, "OTEL_SDK_DISABLED": "true"}, want: false},
		"exporter, no target": {env: map[string]string{exporter: "otlp"}, want: false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if got := telemetry.Enabled(func(k string) string { return tc.env[k] }); got != tc.want {
				t.Errorf("Enabled = %v; want %v", got, tc.want)
			}
		})
	}
}

func TestCardinalityLimit(t *testing.T) {
	t.Parallel()

	if limit, ok := telemetry.CardinalityLimit(func(string) string { return "" }); !ok || limit != telemetry.DefaultCardinalityLimit {
		t.Errorf("limit %d, %v; want %d", limit, ok, telemetry.DefaultCardinalityLimit)
	}

	if _, ok := telemetry.CardinalityLimit(func(k string) string {
		if k == "OTEL_GO_X_CARDINALITY_LIMIT" {
			return "5000"
		}

		return ""
	}); ok {
		t.Error("overrode OTEL_GO_X_CARDINALITY_LIMIT")
	}
}

// Without an endpoint, Setup changes nothing and its shutdown is a no-op.
func TestSetupDisabled(t *testing.T) {
	t.Setenv(endpoint, "")
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", "")

	stop, err := telemetry.Setup(t.Context(), "waterwheel-proxy", "test")
	if err != nil {
		t.Fatal(err)
	}

	if err := stop(t.Context()); err != nil {
		t.Fatal(err)
	}
}

// With an endpoint, Setup builds the exporter and resource, which fails if,
// for one, the resource's schema URLs conflict.
func TestSetupEnabled(t *testing.T) {
	t.Setenv(endpoint, "http://127.0.0.1:1")
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "deployment.environment.name=test")
	t.Setenv("POD_NAME", "bot-1")
	t.Setenv("POD_NAMESPACE", "support")

	stop, err := telemetry.Setup(t.Context(), "waterwheel-proxy", "test")
	if err != nil {
		t.Fatal(err)
	}

	_ = stop(t.Context()) // Flushing to nowhere fails, which doesn't matter here.
}

// What's recorded reaches an OTLP endpoint, at the latest when Setup's
// function flushes it.
func TestSetupExports(t *testing.T) {
	var posts atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/v1/metrics" {
			posts.Add(1)
		}
	}))
	t.Cleanup(srv.Close)

	t.Setenv(endpoint, srv.URL)

	stop, err := telemetry.Setup(t.Context(), "waterwheel-proxy", "test")
	if err != nil {
		t.Fatal(err)
	}

	counter, err := otel.Meter("test").Int64Counter("test.count")
	if err != nil {
		t.Fatal(err)
	}

	counter.Add(t.Context(), 1)

	if err := stop(t.Context()); err != nil {
		t.Fatal(err)
	}

	if posts.Load() == 0 {
		t.Error("nothing reached the OTLP endpoint")
	}
}
