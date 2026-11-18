// SPDX-License-Identifier: Apache-2.0

// Package telemetry sets up OpenTelemetry metrics, configured the standard way:
// the OTEL_* environment variables.
package telemetry

import (
	"context"
	"os"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"
)

// Enabled is whether metrics are to be exported: an OTLP endpoint is set, and
// neither OTEL_SDK_DISABLED nor OTEL_METRICS_EXPORTER=none turns them off.
func Enabled(getenv func(string) string) bool {
	if strings.EqualFold(getenv("OTEL_SDK_DISABLED"), "true") {
		return false
	}

	switch strings.ToLower(getenv("OTEL_METRICS_EXPORTER")) {
	case "", "otlp":
	default:
		return false
	}

	return getenv("OTEL_EXPORTER_OTLP_ENDPOINT") != "" || getenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT") != ""
}

// Setup makes the global meter provider export over OTLP, as the OTEL_*
// variables say, when Enabled. Otherwise metrics stay no-ops. service names
// the process unless OTEL_SERVICE_NAME does; POD_NAME and POD_NAMESPACE, from
// the downward API, describe the pod. The returned function flushes and stops
// the exporter.
func Setup(ctx context.Context, service, version string) (func(context.Context) error, error) {
	if !Enabled(os.Getenv) {
		return func(context.Context) error { return nil }, nil
	}

	exporter, err := newExporter(ctx)
	if err != nil {
		return nil, err
	}

	attrs := []resource.Option{
		resource.WithAttributes(semconv.ServiceName(service), semconv.ServiceVersion(version)),
		resource.WithTelemetrySDK(),
	}
	if pod := os.Getenv("POD_NAME"); pod != "" {
		attrs = append(attrs, resource.WithAttributes(semconv.K8SPodName(pod)))
	}

	if ns := os.Getenv("POD_NAMESPACE"); ns != "" {
		attrs = append(attrs, resource.WithAttributes(semconv.K8SNamespaceName(ns)))
	}

	// Last, so OTEL_SERVICE_NAME and OTEL_RESOURCE_ATTRIBUTES win.
	attrs = append(attrs, resource.WithFromEnv())

	res, err := resource.New(ctx, attrs...)
	if err != nil {
		return nil, err
	}

	opts := []sdkmetric.Option{
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exporter)),
		sdkmetric.WithResource(res),
	}
	if limit, ok := CardinalityLimit(os.Getenv); ok {
		opts = append(opts, sdkmetric.WithCardinalityLimit(limit))
	}

	provider := sdkmetric.NewMeterProvider(opts...)
	otel.SetMeterProvider(provider)

	return provider.Shutdown, nil
}

// DefaultCardinalityLimit is how many attribute sets each metric keeps, unless
// OTEL_GO_X_CARDINALITY_LIMIT says otherwise. A workload picks the
// destinations of its connections, so it could make up endless server.address
// and server.port values; past the limit they count together, in the set
// otel.metric.overflow=true.
const DefaultCardinalityLimit = 250

// CardinalityLimit is the limit to set, and false when
// OTEL_GO_X_CARDINALITY_LIMIT sets it, which the SDK reads itself.
func CardinalityLimit(getenv func(string) string) (int, bool) {
	if getenv("OTEL_GO_X_CARDINALITY_LIMIT") != "" {
		return 0, false
	}

	return DefaultCardinalityLimit, true
}

// newExporter is the OTLP exporter OTEL_EXPORTER_OTLP_PROTOCOL picks,
// http/protobuf by default as the specification says. Each reads the rest of
// its configuration (endpoint, headers, TLS, timeout) from OTEL_* itself.
func newExporter(ctx context.Context) (sdkmetric.Exporter, error) {
	protocol := os.Getenv("OTEL_EXPORTER_OTLP_METRICS_PROTOCOL")
	if protocol == "" {
		protocol = os.Getenv("OTEL_EXPORTER_OTLP_PROTOCOL")
	}

	if protocol == "grpc" {
		return otlpmetricgrpc.New(ctx)
	}

	return otlpmetrichttp.New(ctx)
}
