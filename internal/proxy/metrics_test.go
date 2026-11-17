// SPDX-License-Identifier: Apache-2.0

package proxy_test

import (
	"net"
	"net/http"
	"net/url"
	"strconv"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"go.riptides.io/waterwheel/internal/proxy"
)

// measured is a proxy setup recording metrics into a reader.
func measured(t *testing.T, tokens proxy.Tokens) (*setup, *sdkmetric.ManualReader) {
	t.Helper()

	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))

	return newSetup(t, tokens, func(p *proxy.Proxy) { p.Metrics = proxy.NewMetrics(mp) }), reader
}

// points are the data points of the metric named name: each one's attributes
// and its value (a sum's, or a histogram's count).
func points(t *testing.T, reader *sdkmetric.ManualReader, name string) map[attribute.Distinct]point {
	t.Helper()

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(t.Context(), &rm); err != nil {
		t.Fatal(err)
	}

	got := map[attribute.Distinct]point{}

	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}

			switch d := m.Data.(type) {
			case metricdata.Sum[int64]:
				for _, dp := range d.DataPoints {
					got[dp.Attributes.Equivalent()] = point{dp.Attributes, dp.Value}
				}
			case metricdata.Histogram[float64]:
				for _, dp := range d.DataPoints {
					got[dp.Attributes.Equivalent()] = point{dp.Attributes, int64(dp.Count)}
				}
			case metricdata.Histogram[int64]:
				for _, dp := range d.DataPoints {
					got[dp.Attributes.Equivalent()] = point{dp.Attributes, dp.Sum}
				}
			}
		}
	}

	return got
}

type point struct {
	attrs attribute.Set
	value int64
}

func (p point) attr(key string) string {
	v, _ := p.attrs.Value(attribute.Key(key))

	return v.String()
}

// only is the single data point of name, failing the test without exactly one.
func only(t *testing.T, reader *sdkmetric.ManualReader, name string) point {
	t.Helper()

	pts := points(t, reader, name)
	if len(pts) != 1 {
		t.Fatalf("%s has %d data points; want 1", name, len(pts))
	}

	for _, p := range pts {
		return p
	}

	return point{}
}

func hostPort(t *testing.T, raw string) (string, string) {
	t.Helper()

	u, _ := url.Parse(raw)
	host, port, _ := net.SplitHostPort(u.Host)

	return host, port
}

func TestMetricsOfAnInterceptedRequest(t *testing.T) {
	t.Parallel()

	s, reader := measured(t, staticTokens{tok: testToken})

	if status, _ := call(t, s.client, s.bound.URL); status != http.StatusOK {
		t.Fatalf("status %d", status)
	}

	host, port := hostPort(t, s.bound.URL)
	p := only(t, reader, "http.client.request.duration")

	for key, want := range map[string]string{
		"http.request.method":       "POST",
		"http.response.status_code": "200",
		"server.address":            host,
		"server.port":               port,
		"url.scheme":                "https",
		"waterwheel.binding":        "support/bot-anthropic",
		"waterwheel.provider":       "anthropic",
		"error.type":                "",
	} {
		if got := p.attr(key); got != want {
			t.Errorf("%s = %q; want %q", key, got, want)
		}
	}

	if p.value != 1 {
		t.Errorf("%d requests recorded; want 1", p.value)
	}

	if size := only(t, reader, "http.client.response.body.size"); size.value == 0 {
		t.Error("no response body size recorded")
	}
}

func TestMetricsOfARefusedRequest(t *testing.T) {
	t.Parallel()

	s, reader := measured(t, staticTokens{err: errDenied})

	if status, _ := call(t, s.client, s.bound.URL); status != http.StatusForbidden {
		t.Fatalf("status %d", status)
	}

	p := only(t, reader, "waterwheel.proxy.refused_requests")
	if p.value != 1 || p.attr("waterwheel.binding") != "support/bot-anthropic" {
		t.Errorf("refusals %d for %q; want 1 for support/bot-anthropic", p.value, p.attr("waterwheel.binding"))
	}

	if pts := points(t, reader, "http.client.request.duration"); len(pts) != 0 {
		t.Errorf("a refused request recorded as sent upstream: %v", pts)
	}
}

func TestMetricsOfATunnel(t *testing.T) {
	t.Parallel()

	s, reader := measured(t, staticTokens{tok: testToken})

	if status, _ := call(t, s.client, s.other.URL); status != http.StatusOK {
		t.Fatalf("status %d", status)
	}

	// The tunnel is measured once the app's connection closes.
	s.client.CloseIdleConnections()

	deadline := time.Now().Add(5 * time.Second)
	for len(points(t, reader, "waterwheel.proxy.tunnel.duration")) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}

	host, port := hostPort(t, s.other.URL)

	p := only(t, reader, "waterwheel.proxy.tunnel.duration")
	if p.attr("server.address") != host || p.attr("server.port") != port || p.attr("error.type") != "" {
		t.Errorf("tunnel to %s:%s (error %q); want %s:%s", p.attr("server.address"), p.attr("server.port"), p.attr("error.type"), host, port)
	}

	io := points(t, reader, "waterwheel.proxy.tunnel.io")
	if len(io) != 2 {
		t.Fatalf("tunnel.io has %d data points; want transmit and receive", len(io))
	}

	for _, d := range io {
		if d.value == 0 {
			t.Errorf("no bytes for network.io.direction=%s", d.attr("network.io.direction"))
		}
	}

	if _, err := strconv.Atoi(port); err != nil {
		t.Fatal(err)
	}
}
