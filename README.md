# vk2tg

`vk2tg` is an independent Go project and the primary production relay for one
configured VK peer and one Telegram destination (chat or channel). It is strictly
one-way: VK User Long Poll -> Telegram Bot API. VK access is read-only;
Telegram is outbound-only, with no polling, webhooks or reverse relay.

## Features

- Target-peer messages are accepted regardless of `Out`, including `Out=1`
  and the token account's own messages. Sender blocklist and empty-content
  filtering still apply.
- Text, photos/albums, documents, stickers, wall reposts, links and mapped replies.
  Stickers prefer VK `images` and are uploaded with `sendPhoto`.
- SQLite mappings for duplicate suppression and replies across restarts.
- Oldest-first full history migration with pagination, persistent mappings and
  guarded resume; a separate nonpersistent latest-message replay command.
- Central VK API pacing/backoff and flood protection for errors 6/9/29;
  Telegram pacing and `429`/`retry_after` handling.
- Structured JSON logs and optional privacy-safe `DEBUG=true` traces.
- Optional build-time message observers with memory queues or opt-in SQLite retries;
  disabled by default, with diagnostic `example` and Prometheus `metrics` plugins.

## Quick Start

Use Go 1.27.1 or newer. Create a private environment file without overwriting
an existing one, then replace the template credentials and numeric IDs:

```sh
test -e .env || cp .env.example .env
chmod 600 .env
```

The binary reads environment variables, not `.env` itself. Build and start in
dry-run mode first (this still reads VK; it does not send Telegram messages):

```sh
go build -o vk2tg .
(
  set -a
  . ./.env
  set +a
  export DRY_RUN=true
  exec ./vk2tg
)
```

Set `DRY_RUN=false` only for intended delivery. With no arguments, the binary
starts live Long Poll and does not replay history. Run only one delivery process
per source/destination; do not run a second instance alongside production.

## Operations

The included [Compose deployment](compose.yaml) enables real delivery, uses
SQLite at `/data/state.db`, restarts with `unless-stopped`, and rotates Docker
`json-file` logs at `10m` x `3` per container.

- [Runtime and operations](docs/runtime.md): environment variables, media,
  SQLite, rate protection, debug/privacy, deployment and health checks.
- [Plugins](docs/plugins.md): registration, local-only files, configuration,
  lifecycle and delivery guarantees.
- [Metrics](plugins/metrics/README.md): optional `/metrics`, Docker access and Prometheus setup.
- [History](docs/history.md): `test-history --count N` versus `migrate-history`,
  pagination, backups, resume and failure handling.

Long Poll has no durable cursor. Downtime can lose events, and delivery is not
exactly-once. History migration detects ambiguous pending sends and stops for
reconciliation rather than automatically resending them.

## Development

Tests use mock HTTP APIs and temporary SQLite databases, not live accounts.
Each plugin owns its code, tests, README and optional Compose overlay in
`plugins/<name>/`. Shared runtime code and tests live in `internal/pluginruntime`;
shared test helpers live in `internal/plugintest`. Public tests are versioned and
run directly in a fresh checkout. Private plugin directories and their local
registration imports remain excluded from Git.

```sh
gofmt -l *.go plugins internal
go test ./...
go test -race ./...
go vet ./...
go build -o /tmp/vk2tg-check .
git diff --check
```

`scripts/test.sh` remains a compatibility wrapper around `go test`/`go vet`;
it no longer copies tests or requires files outside the checkout.
