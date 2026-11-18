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
`proxy.env` can't set the sidecar's own configuration (the `WATERWHEEL_*`
variables and `SPIFFE_ENDPOINT_SOCKET`): the controller refuses to start. A
value can refer to `$(POD_NAME)` and `$(POD_NAMESPACE)`, the workload's pod.

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
| `http.client.open_connections` | updowncounter | `{connection}` | The proxy's connections to bound destinations, `active` while one carries a request, `idle` otherwise. |
| `http.client.connection.duration` | histogram | s | How long each of those connections lasted. |
| `waterwheel.proxy.refused_requests` | counter | `{request}` | Requests to a bound destination refused for want of a token. |
| `waterwheel.proxy.token_exchanges` | counter | `{exchange}` | Token exchanges and refreshes, failed ones with `error.type`. |
| `waterwheel.proxy.tunnel.duration` | histogram | s | Connections to any other destination, passed through untouched, or failing to open. |
| `waterwheel.proxy.tunnel.io` | counter | By | Bytes through those connections. |

Attributes:

- `http.request.method`, `http.response.status_code`, `url.scheme`: the HTTP
  conventions'. A method they don't know is `_OTHER`.
- `network.protocol.name`, `network.protocol.version`: `http`, and `1.1` or
  `2`, as the proxy spoke it upstream.
- `http.connection.state`: `active` or `idle`, on open connections.
- `server.address`, `server.port`: the destination the app asked for.
- `network.peer.address`: the IP address the proxy connected to for it, on
  connections and tunnels.
- `error.type`: why it failed. The status code for a response of 400 or more;
  `aborted` for one that broke off mid-stream, with its status code;
  `timeout`, `canceled`, `dns`, `dial`, `read` or `write` when there was none;
  `refused` for a token exchange the token endpoint refused; otherwise
  `_OTHER`.
- `network.io.direction`: `transmit` to the destination, `receive` from it.
- `waterwheel.binding`: the CredentialBinding, as `namespace/name`.
- `waterwheel.provider`: the kind of token endpoint, `anthropic`, `openai`,
  `gcp`, `azure`, `rfc7523` or `rfc8693`.

Durations use the HTTP conventions' buckets, which end at 10 seconds,
extended with 30, 60, 120 and 300 for slow APIs such as LLMs.

Each metric keeps at most 250 attribute sets per pod, since an app picks the
destinations it connects to. Past that, new ones count together under
`otel.metric.overflow=true`. `OTEL_GO_X_CARDINALITY_LIMIT` changes the limit.

Traffic that doesn't go through the proxy (an app that ignores `HTTPS_PROXY`,
plain TCP, or inbound connections) isn't measured.
