// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"net"
	"testing"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// A connection closed before it carried a request, such as one whose TLS
// handshake failed, wasn't established, so it isn't measured.
func TestUnusedConnectionIsNotMeasured(t *testing.T) {
	t.Parallel()

	reader := sdkmetric.NewManualReader()
	m := NewMetrics(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))

	a, b := net.Pipe()
	t.Cleanup(func() { _ = b.Close() })

	c := &upstreamConn{Conn: a, metrics: m, host: "api.example.com", port: 443}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(t.Context(), &rm); err != nil {
		t.Fatal(err)
	}

	for _, sm := range rm.ScopeMetrics {
		for _, metric := range sm.Metrics {
			t.Errorf("%s recorded for a connection that carried no request", metric.Name)
		}
	}
}
