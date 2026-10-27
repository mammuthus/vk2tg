# Metrics observer

The optional `metrics` plugin exposes `GET /metrics` in Prometheus text format
and supports OpenMetrics negotiation. Its implementation is in
[`plugins/metrics.go`](../plugins/metrics.go), using the official
[Prometheus Go client](https://github.com/prometheus/client_golang).

It observes successful live deliveries, stores counters in memory, and uses no
SQLite database. It does not query VK or Telegram. Like other observers, it is
not started by dry-run or history commands.

## Enable

Add `metrics` to the existing `PLUGINS` list in your private `.env`, for example:

```dotenv
PLUGINS=example,metrics
METRICS_LISTEN_ADDR=127.0.0.1:9091
```

For a directly launched binary, export these variables before starting it.
`METRICS_LISTEN_ADDR` defaults to `127.0.0.1:9091` and is only read when this plugin
initializes. No listener is opened while `metrics` is disabled.

For Docker, the optional [`compose.metrics.yaml`](../compose.metrics.yaml)
overlay sets the **container** listener to `0.0.0.0:9091` and publishes it only on
**host** `127.0.0.1:9091` by default. Set `METRICS_HOST_PORT` in `.env` to use
another host port (and use that port in curl and Prometheus targets). The overlay
overrides `METRICS_LISTEN_ADDR` from `.env`. Apply it
when an update of the live service is intended:

```sh
docker compose -f compose.yaml -f compose.metrics.yaml up -d --build --no-deps vk2tg
curl --fail http://127.0.0.1:9091/metrics
```

Use the same pair of Compose files for subsequent updates. Do not start a second
relay process. To disable the plugin, remove `metrics` from `PLUGINS` and recreate
the container; to also remove port publication, use only the base Compose file.
Base Compose continues to publish no ports.

If the deployment uses additional private observers or Compose overlays,
preserve their `PLUGINS` entries and `-f` files during updates. The example above
must not replace an existing deployment's complete configuration.

If the address is invalid or the port is occupied inside the container, plugin
initialization fails with a sanitized warning and the relay continues. A host
port conflict prevents Docker from starting that container; choose an available
host port with `METRICS_HOST_PORT` before applying it. `metrics plugin initialized` marks
successful listener setup. Shutdown releases the listener and stops HTTP service.

## Exported metrics

| Metric | Meaning |
| --- | --- |
| `vk2tg_observed_messages_total` | Source-message events processed by this observer after successful delivery and mapping commit |
| `vk2tg_observed_attachments_total{kind}` | Normalized attachments in those events; fixed `kind` values `photo`, `document`, `sticker`, `other` |
| `vk2tg_last_observed_relay_timestamp_seconds` | Latest observed relay timestamp in Unix seconds; `0` until an event with a valid timestamp arrives |
| `go_*`, `process_*` | Standard Go runtime and process collectors: goroutines, memory, CPU, process start time and related values |

One source message counts once even if sent as an album or multiple Telegram
chunks. Unsupported-attachment placeholders are text, not attachment assets, so
are not added to the attachment counter. Unknown future attachment kinds map to
`other`. Scraping does not increment event counters. Message text, IDs, filenames
and URLs are never used as labels or exported values.

Application counters reset on each plugin/process restart. The memory queue is
best-effort: overflow or shutdown can lose pending events, so these counters are
not an exact audit of every Telegram delivery. API errors, request latency,
rejected messages and queue-drop totals are not available through the current
event contract. `/metrics` availability alone does not prove VK Long Poll is
healthy; an idle source can legitimately leave the last-relay timestamp unchanged.

## Prometheus scrape example

For Prometheus running directly on the same host:

```yaml
scrape_configs:
  - job_name: vk2tg
    scrape_interval: 15s
    static_configs:
      - targets: ['127.0.0.1:9091']
```

A separate Prometheus container or remote host cannot use this loopback target
as-is; configure a reachable private container network or host address explicitly.
The endpoint has no authentication. Prometheus/Grafana deployment and network
exposure beyond host loopback are separate configuration tasks.

Local tests in `tests/plugins/metrics_test.go` (ignored by Git) use synthetic
events and local ephemeral listeners. They validate exposition, counts, bounded
labels, concurrent scrapes, lifecycle, startup failure isolation and disabled
operation without sending messages to Telegram.

Run the full suite with `./scripts/test.sh test ./...`; see [Plugins](plugins.md)
for the source/test layout.
