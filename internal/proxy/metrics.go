// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"slices"
	"strconv"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"

	"go.riptides.io/waterwheel/internal/bindings"
)

// Attributes of Waterwheel's own, beside OpenTelemetry's semantic conventions.
const (
	// BindingKey is the CredentialBinding, as namespace/name.
	BindingKey = attribute.Key("waterwheel.binding")
	// ProviderKey is the kind of token endpoint: anthropic, openai, gcp,
	// azure, rfc7523 or rfc8693.
	ProviderKey = attribute.Key("waterwheel.provider")
)

// Metrics are what the proxy measures. Intercepted requests follow
// OpenTelemetry's HTTP client conventions; tunnels, token exchanges and
// refused requests are Waterwheel's own.
type Metrics struct {
	requestDuration  metric.Float64Histogram
	requestBodySize  metric.Int64Histogram
	responseBodySize metric.Int64Histogram
	refused          metric.Int64Counter
	tunnelDuration   metric.Float64Histogram
	tunnelIO         metric.Int64Counter
	exchanges        metric.Int64Counter
}

// durationBuckets are the HTTP semantic conventions' for durations in seconds,
// which end at 10, extended to 5 minutes: LLM APIs often take longer.
var durationBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.075, 0.1, 0.25, 0.5, 0.75, 1, 2.5, 5, 7.5, 10, 30, 60, 120, 300}

// NewMetrics creates the instruments with mp, or with the global meter
// provider if mp is nil. A nil *Metrics records nothing.
func NewMetrics(mp metric.MeterProvider) *Metrics {
	if mp == nil {
		mp = otel.GetMeterProvider()
	}

	meter := mp.Meter("go.riptides.io/waterwheel/internal/proxy")

	// The instrument options are valid, and a no-op instrument stands in for
	// any that the SDK still refuses, so the errors can go.
	m := &Metrics{}
	m.requestDuration, _ = meter.Float64Histogram("http.client.request.duration",
		metric.WithUnit("s"), metric.WithDescription("Duration of HTTP client requests."),
		metric.WithExplicitBucketBoundaries(durationBuckets...))
	m.requestBodySize, _ = meter.Int64Histogram("http.client.request.body.size",
		metric.WithUnit("By"), metric.WithDescription("Size of HTTP client request bodies."))
	m.responseBodySize, _ = meter.Int64Histogram("http.client.response.body.size",
		metric.WithUnit("By"), metric.WithDescription("Size of HTTP client response bodies."))
	m.refused, _ = meter.Int64Counter("waterwheel.proxy.refused_requests",
		metric.WithUnit("{request}"), metric.WithDescription("Requests to a bound destination refused for want of a token."))
	m.tunnelDuration, _ = meter.Float64Histogram("waterwheel.proxy.tunnel.duration",
		metric.WithUnit("s"), metric.WithDescription("Duration of connections passed through untouched."),
		metric.WithExplicitBucketBoundaries(durationBuckets...))
	m.tunnelIO, _ = meter.Int64Counter("waterwheel.proxy.tunnel.io",
		metric.WithUnit("By"), metric.WithDescription("Bytes through connections passed through untouched."))
	m.exchanges, _ = meter.Int64Counter("waterwheel.proxy.token_exchanges",
		metric.WithUnit("{exchange}"), metric.WithDescription("Token exchanges, and their failures in error.type."))

	return m
}

func bindingAttrs(b bindings.Binding) []attribute.KeyValue {
	return []attribute.KeyValue{BindingKey.String(b.Namespace + "/" + b.Name), ProviderKey.String(providerKind(b))}
}

// providerKind is a binding's kind of token endpoint, a short stable name.
func providerKind(b bindings.Binding) string {
	p := b.Provider

	switch {
	case p.Anthropic != nil:
		return "anthropic"
	case p.OpenAI != nil:
		return "openai"
	case p.GCP != nil:
		return "gcp"
	case p.Azure != nil:
		return "azure"
	case p.RFC7523 != nil:
		return "rfc7523"
	case p.RFC8693 != nil:
		return "rfc8693"
	}

	return "unknown"
}

// request records a request the proxy sent upstream for b. status is 0 when
// err left it without a response; with a status, err broke the response off.
func (m *Metrics) request(ctx context.Context, b bindings.Binding, method, host string, port, status int, err error,
	took time.Duration, sent, received int64,
) {
	if m == nil {
		return
	}

	attrs := append(bindingAttrs(b),
		methodAttr(method),
		semconv.ServerAddress(host),
		semconv.ServerPort(port),
		semconv.URLScheme("https"))

	switch {
	case err != nil:
		attrs = append(attrs, semconv.ErrorTypeKey.String(errorType(err)))
		if status != 0 {
			attrs = append(attrs, semconv.HTTPResponseStatusCode(status))
		}
	case status >= 400:
		attrs = append(attrs, semconv.HTTPResponseStatusCode(status), semconv.ErrorTypeKey.String(strconv.Itoa(status)))
	default:
		attrs = append(attrs, semconv.HTTPResponseStatusCode(status))
	}

	set := metric.WithAttributes(attrs...)
	m.requestDuration.Record(ctx, took.Seconds(), set)
	m.requestBodySize.Record(ctx, sent, set)

	if status != 0 {
		m.responseBodySize.Record(ctx, received, set)
	}
}

// refusal records a request to b's destination refused for want of a token.
func (m *Metrics) refusal(ctx context.Context, b bindings.Binding) {
	if m == nil {
		return
	}

	m.refused.Add(ctx, 1, metric.WithAttributes(bindingAttrs(b)...))
}

// tunnel records a passed-through connection to host:port, or the failure to
// open it.
func (m *Metrics) tunnel(ctx context.Context, host string, port int, err error, took time.Duration, sent, received int64) {
	if m == nil {
		return
	}

	attrs := []attribute.KeyValue{semconv.ServerAddress(host), semconv.ServerPort(port)}
	if err != nil {
		attrs = append(attrs, semconv.ErrorTypeKey.String(errorType(err)))
	}

	m.tunnelDuration.Record(ctx, took.Seconds(), metric.WithAttributes(attrs...))

	if err != nil {
		return
	}

	m.tunnelIO.Add(ctx, sent, metric.WithAttributes(slices.Concat(attrs, []attribute.KeyValue{semconv.NetworkIODirectionTransmit})...))
	m.tunnelIO.Add(ctx, received, metric.WithAttributes(slices.Concat(attrs, []attribute.KeyValue{semconv.NetworkIODirectionReceive})...))
}

// exchange records a token exchange for b, failed if err isn't nil.
func (m *Metrics) exchange(ctx context.Context, b bindings.Binding, err error) {
	if m == nil {
		return
	}

	attrs := bindingAttrs(b)
	if err != nil {
		kind := errorType(err)
		if denied(err) {
			kind = "refused"
		}

		attrs = append(attrs, semconv.ErrorTypeKey.String(kind))
	}

	m.exchanges.Add(ctx, 1, metric.WithAttributes(attrs...))
}

// methodAttr is http.request.method, _OTHER for a method the HTTP
// conventions don't know, so a client can't make up endless values.
func methodAttr(method string) attribute.KeyValue {
	switch method {
	case http.MethodConnect:
		return semconv.HTTPRequestMethodConnect
	case http.MethodDelete:
		return semconv.HTTPRequestMethodDelete
	case http.MethodGet:
		return semconv.HTTPRequestMethodGet
	case http.MethodHead:
		return semconv.HTTPRequestMethodHead
	case http.MethodOptions:
		return semconv.HTTPRequestMethodOptions
	case http.MethodPatch:
		return semconv.HTTPRequestMethodPatch
	case http.MethodPost:
		return semconv.HTTPRequestMethodPost
	case http.MethodPut:
		return semconv.HTTPRequestMethodPut
	case http.MethodTrace:
		return semconv.HTTPRequestMethodTrace
	case "QUERY":
		return semconv.HTTPRequestMethodQuery
	}

	return semconv.HTTPRequestMethodOther
}

// errorType is a short, low-cardinality name for err, for error.type.
func errorType(err error) string {
	switch {
	case errors.Is(err, errAborted):
		return "aborted"
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, os.ErrDeadlineExceeded), errors.Is(err, errExchangeTimeout):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	}

	if _, ok := errors.AsType[*net.DNSError](err); ok {
		return "dns"
	}

	if op, ok := errors.AsType[*net.OpError](err); ok {
		return op.Op
	}

	return semconv.ErrorTypeOther.Value.AsString()
}
