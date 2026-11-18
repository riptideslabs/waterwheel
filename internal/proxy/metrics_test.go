// SPDX-License-Identifier: Apache-2.0

package proxy_test

import (
	"io"
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

// requestDuration is the HTTP client conventions' request duration metric.
const requestDuration = "http.client.request.duration"

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

// waitFor waits for each metric in want to have at least that many data
// points: the proxy records once the app has had its response, and the
// instruments don't record together.
func waitFor(t *testing.T, reader *sdkmetric.ManualReader, want map[string]int) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)

	for name, n := range want {
		for len(points(t, reader, name)) < n {
			if time.Now().After(deadline) {
				t.Fatalf("%s has %d data points; want %d", name, len(points(t, reader, name)), n)
			}

			time.Sleep(10 * time.Millisecond)
		}
	}
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

	waitFor(t, reader, map[string]int{
		requestDuration: 1, "http.client.request.body.size": 1, "http.client.response.body.size": 1,
	})

	host, port := hostPort(t, s.bound.URL)
	p := only(t, reader, requestDuration)

	for key, want := range map[string]string{
		"http.request.method":       "POST",
		"http.response.status_code": "200",
		"server.address":            host,
		"server.port":               port,
		"url.scheme":                "https",
		"waterwheel.binding":        "support/bot-anthropic",
		"waterwheel.provider":       "anthropic",
		"error.type":                "",
		"network.protocol.name":     "http",
		"network.protocol.version":  "2",
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

	if pts := points(t, reader, requestDuration); len(pts) != 0 {
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

	waitFor(t, reader, map[string]int{"waterwheel.proxy.tunnel.duration": 1, "waterwheel.proxy.tunnel.io": 2})

	host, port := hostPort(t, s.other.URL)

	p := only(t, reader, "waterwheel.proxy.tunnel.duration")
	if p.attr("network.peer.address") != host {
		t.Errorf("network.peer.address = %q; want %s", p.attr("network.peer.address"), host)
	}

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

// A response that breaks off mid-stream is recorded, as failed, before the
// proxy breaks off its own.
func TestMetricsOfAResponseThatBrokeOff(t *testing.T) {
	t.Parallel()

	s, reader := measured(t, staticTokens{tok: testToken})

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, s.bound.URL+"/v1/broken", nil)
	if err != nil {
		t.Fatal(err)
	}

	resp, err := s.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := io.ReadAll(resp.Body); err == nil {
		t.Error("the app read the whole response; want it broken off")
	}

	_ = resp.Body.Close()

	waitFor(t, reader, map[string]int{requestDuration: 1})

	p := only(t, reader, requestDuration)
	if p.attr("error.type") != "aborted" || p.attr("http.response.status_code") != "200" || p.attr("http.request.method") != "GET" {
		t.Errorf("error.type %q, status %q, method %q; want aborted, 200, GET",
			p.attr("error.type"), p.attr("http.response.status_code"), p.attr("http.request.method"))
	}
}

// The proxy's connections to bound destinations: active while they carry a
// request, idle after, and how long they lasted once closed.
func TestMetricsOfConnections(t *testing.T) {
	t.Parallel()

	s, reader := measured(t, staticTokens{tok: testToken})

	if status, _ := call(t, s.client, s.bound.URL); status != http.StatusOK {
		t.Fatalf("status %d", status)
	}

	waitFor(t, reader, map[string]int{requestDuration: 1})

	host, port := hostPort(t, s.bound.URL)
	open := map[string]int64{}

	for _, p := range points(t, reader, "http.client.open_connections") {
		if p.attr("server.address") != host || p.attr("server.port") != port || p.attr("network.peer.address") != host ||
			p.attr("network.protocol.version") != "2" || p.attr("url.scheme") != "https" {
			t.Errorf("connection attributes %v", p.attrs.ToSlice())
		}

		open[p.attr("http.connection.state")] = p.value
	}

	if open["active"] != 0 || open["idle"] != 1 {
		t.Errorf("open connections %v; want one idle", open)
	}

	upstream, ok := s.proxy.Upstream.(*http.Transport)
	if !ok {
		t.Fatal("the proxy's upstream isn't an http.Transport")
	}

	upstream.CloseIdleConnections()
	waitFor(t, reader, map[string]int{"http.client.connection.duration": 1})

	if p := only(t, reader, "http.client.connection.duration"); p.value != 1 || p.attr("network.peer.address") != host {
		t.Errorf("%d closed connections to %q; want 1 to %s", p.value, p.attr("network.peer.address"), host)
	}

	for _, p := range points(t, reader, "http.client.open_connections") {
		if p.value != 0 {
			t.Errorf("%s connections %d after closing; want 0", p.attr("http.connection.state"), p.value)
		}
	}
}
