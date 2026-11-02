# Build-time observers

Plugins observe successfully relayed **live** messages. They do not filter,
modify, or cancel Telegram delivery. An empty `PLUGINS` (the default) starts no
workers. History commands and dry-run never start observers or enqueue events.
The repository includes the diagnostic `example` observer and a
[Prometheus-compatible `metrics` observer](../plugins/metrics/README.md).

## Source layout

- `plugins/register.go`: imports the public plugin packages for registration.
- `plugins/example/`: example implementation, unit tests and README.
- `plugins/metrics/`: metrics implementation, unit tests, README and `compose.yaml`.
- `internal/pluginruntime/plugin_api.go`: event contract and registration.
- `internal/pluginruntime/plugin_manager.go`: lifecycle and memory delivery.
- `internal/pluginruntime/plugin_queue.go`: optional SQLite delivery.
- `internal/pluginruntime/plugin_manager_test.go`: runtime regression tests.
- `internal/plugintest/`: shared helpers imported only by plugin tests.

The executable imports `plugins` for registration. Implementations import the
runtime package; the runtime has no dependency on concrete plugins. Each plugin
has its own Go package. Core bot integration tests remain beside the root
application files. All public tests are versioned alongside their packages.

## Adding and enabling an observer

Create `plugins/myplugin/` with `package myplugin`. Keep implementation, unit
tests, README, example configuration and any Compose overlay in that directory.
Register a factory from
`init()` using `pluginruntime.Register("myplugin", factory)`. Import
`github.com/mammuthus/vk2tg/internal/pluginruntime`; the factory returns a
`pluginruntime.Handler`:

```go
func init() {
    pluginruntime.Register("myplugin", func() pluginruntime.Handler {
        return &myPlugin{}
    })
}

// myPlugin implements Handle(context.Context, pluginruntime.Event) error.
```

Add a blank import of `github.com/mammuthus/vk2tg/plugins/myplugin` to
`plugins/register.go` so the executable links and registers the package.

`pluginruntime.Register` selects a memory queue; no plugin SQLite database is
opened. To opt into persistent delivery and retries, use
`pluginruntime.RegisterDurable("myplugin", factory)` instead. These are two delivery modes for the same observer interface.

Optionally implement `Init(context.Context, pluginruntime.Settings) error` and
`Close(context.Context) error`. Keep `init()` limited to registration and
factories free of I/O; perform setup in `Init`. Settings provides:

- `Logger`: the bot's logger, already tagged with the plugin name. Never log
  message bodies, IDs, filenames, URLs, credentials or raw external errors.
- `Lifetime`: a context lasting until the worker stops. Use it for background
  services; the context passed to `Init` expires as soon as that call finishes.
- `DataDir`: a private path under `plugins/<name>` beside the bot's state DB.
  Create it with mode `0700` only if needed. The framework does not create it.

An observer can start an HTTP server in `Init`, update counters in `Handle`, and
stop its server in `Close`. Protect counters shared with HTTP handlers against
data races. Publishing a port from Docker requires an explicit deployment
configuration change. The example does not expose an HTTP endpoint.

Build the binary or Docker image, then configure:

```text
PLUGINS=myplugin
PLUGIN_TIMEOUT_SECONDS=10
```

Names are comma-separated, distinct, case-sensitive, and must match
`[a-z][a-z0-9_-]{0,63}`. Unknown names fail configuration validation. Timeout
accepts 1–60 seconds and bounds construction, initialization, and each handler
call. Cleanup has a fresh context capped at five seconds. Implementations must
honor cancellation. Code changes require rebuilding; environment changes
require recreating the container. There is no runtime `.go`/`.so` loader.

For a personal implementation, first exclude `/plugins/myplugin/` in
`.git/info/exclude`. Keep all its files, including tests and configuration,
inside that directory. Import it from `plugins/local_myplugin.go` with
`package plugins`; the `local_*.go` registration bridges are ignored by
`.gitignore`. Do not add a private import to the public `register.go`.

Verify both exclusions with `git check-ignore -v plugins/myplugin/myplugin.go
plugins/local_myplugin.go`. Ignoring does not untrack committed files or prevent
`git add -f`; inspect changes before publishing and back up private files
separately. Docker builds include nested plugin Go source, including ignored
implementations, but exclude tests, documentation and configuration directories.
Only publish images built from public source if personal code must stay private.

Compose commands must run from the repository root with the base file first:
`docker compose -f compose.yaml -f plugins/myplugin/compose.yaml ...`.
Relative bind paths in overlays are resolved against that base project, not the
plugin directory. Preserve all overlays already used by the deployment.

## Example

The [example plugin](../plugins/example/README.md) demonstrates registration,
`Init`, `Handle`, and `Close`. Its implementation and tests are kept together;
it needs no separate Compose overlay.

## Events and memory delivery

`Event` version 1 optionally includes `VKConversationMessageID`
(`vk_conversation_message_id`) when source details are available. Older queued
events omit it and remain valid. It contains original and normalized text (before HTML rendering),
VK message/peer/sender/reply IDs, outgoing flag, source and relay timestamps,
canonical Telegram message/chat IDs, attachment metadata, and counts of
unsupported attachments and ignored forwards. `PublishedAt` is zero when
unavailable; media bytes and credentials are not passed. Existing unsupported
media and forwarding limitations remain unchanged.

Events are independent copies, emitted after full delivery and mapping commit.
Rejected/empty messages, mapped duplicates, incomplete deliveries and API errors
do not produce events. Each observer has a serial worker, independent of others.

The default queue holds up to 64 pending events per observer. Enqueue does not
wait for `Handle`. When full, the new event is dropped with a warning. Handler
errors/panics are logged without raw payloads; failed events are not retried.
Pending memory events are discarded on shutdown or worker failure. There is no
queue deduplication or restart recovery in this mode; the relay's mapping still
suppresses already-delivered source messages. Memory-based metrics are therefore
best-effort counters, not an exact audit of Telegram deliveries.

Initialization failure or a timed-out callback disables that worker until
restart. No further callbacks or concurrent cleanup run on a potentially stuck
instance. Its lifetime context is cancelled. Plugins are trusted code, not a
sandbox: they must follow the relay's privacy and read-only VK rules.

## Optional durable delivery

Only enabled `RegisterDurable` observers use `<STATE_DB_PATH>.plugins.sqlite`
(mode `0600`). One process must own this queue. Queue startup failures disable
durable observers while memory observers and the relay continue. Enqueue failures
never retry Telegram delivery. The durable enqueue wait is bounded to 200 ms.

Committed jobs survive restarts, with independent retries at delays of 1–256
seconds. Later ready jobs can overtake delayed jobs. A crash after a side effect
but before acknowledgement can repeat processing: handlers must be idempotent.
Acknowledgement erases the payload but retains its deduplication key. Pending
jobs contain message text and URLs; protect the queue and backups accordingly.

Capture remains best-effort: a crash between mapping commit and enqueue, or
unavailable storage, can lose an event. There is no automatic history backfill.
Worker failure retains pending/new durable jobs for a later restart.

Switching a plugin to memory mode does not replay, modify or delete its old
SQLite jobs. Re-enabling durable mode under the same name resumes those jobs.
Existing queue files are left untouched when only memory observers are enabled.

## Checks

Run `go test ./...`, `go test -race ./...`, `go vet ./...`, and `go build .`
directly from the repository root. The optional `scripts/test.sh` wrapper
delegates to the same commands without copying files. A public checkout includes
core, runtime and public plugin tests. Local private packages are also tested
when present, without requiring them in a public checkout.

Tests use mock APIs and temporary databases. They cover memory delivery,
overflow, event isolation, lifecycle, failures, optional persistence, retries,
restart recovery and exclusion from history/dry-run modes. Docker excludes
`*_test.go`; test helpers are not imported by the production binary.
