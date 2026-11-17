# Metrics

waterwheel-proxy exports OpenTelemetry metrics over OTLP. They're off until an
OTLP endpoint is set, and configured the standard way, with the `OTEL_*`
environment variables. The chart passes them to every injected sidecar:

```yaml
proxy:
  env:
  - name: OTEL_EXPORTER_OTLP_ENDPOINT
    value: http://otel-collector.observability:4318
```

Any [OTLP exporter variable](https://opentelemetry.io/docs/specs/otel/protocol/exporter/)
works: `OTEL_EXPORTER_OTLP_PROTOCOL` (`http/protobuf` by default, or `grpc`),
`OTEL_EXPORTER_OTLP_HEADERS`, `OTEL_EXPORTER_OTLP_CERTIFICATE`,
`OTEL_METRIC_EXPORT_INTERVAL`, `OTEL_RESOURCE_ATTRIBUTES`, and the rest.
`OTEL_SDK_DISABLED=true` or `OTEL_METRICS_EXPORTER=none` turns them off again.
Variables the sidecar needs itself can't be overridden.

The resource is `service.name=waterwheel-proxy` (unless `OTEL_SERVICE_NAME`
says otherwise), `service.version`, `k8s.pod.name` and `k8s.namespace.name`.

## What's measured

Requests to a bound destination follow OpenTelemetry's
[HTTP client conventions](https://opentelemetry.io/docs/specs/semconv/http/http-metrics/):
they're the requests the proxy sends upstream with a token.

| Metric | Type | Unit | |
|---|---|---|---|
| `http.client.request.duration` | histogram | s | Each request to a bound destination. |
| `http.client.request.body.size` | histogram | By | Its request body. |
| `http.client.response.body.size` | histogram | By | Its response body, when there was a response. |
| `waterwheel.proxy.refused_requests` | counter | `{request}` | Requests to a bound destination refused for want of a token. |
| `waterwheel.proxy.token_exchanges` | counter | `{exchange}` | Token exchanges and refreshes, failed ones with `error.type`. |
| `waterwheel.proxy.tunnel.duration` | histogram | s | Connections to any other destination, passed through untouched, or failing to open. |
| `waterwheel.proxy.tunnel.io` | counter | By | Bytes through those connections. |

Attributes:

- `http.request.method`, `http.response.status_code`, `url.scheme`: the HTTP
  conventions'.
- `server.address`, `server.port`: the destination the app asked for.
- `error.type`: why it failed. The status code for a response of 400 or more;
  `timeout`, `canceled`, `dns`, `dial`, `read` or `write` when there was none;
  `refused` for a token exchange the token endpoint refused; otherwise
  `_OTHER`.
- `network.io.direction`: `transmit` to the destination, `receive` from it.
- `waterwheel.binding`: the CredentialBinding, as `namespace/name`.
- `waterwheel.provider`: the kind of token endpoint, `anthropic`, `openai`,
  `gcp`, `azure`, `rfc7523` or `rfc8693`.

Durations use the HTTP conventions' buckets, which end at 10 seconds,
extended with 30, 60, 120 and 300 for slow APIs such as LLMs.

Traffic that doesn't go through the proxy (an app that ignores `HTTPS_PROXY`,
plain TCP, or inbound connections) isn't measured.
