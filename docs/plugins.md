# Build-time observers

Plugins observe successfully relayed **live** messages. They do not filter,
modify, or cancel Telegram delivery. An empty `PLUGINS` (the default) starts no
workers. History commands and dry-run never start observers or enqueue events.
The repository includes the diagnostic `example` observer and a
[Prometheus-compatible `metrics` observer](metrics.md).

## Source layout

- `plugins/example.go`, `plugins/metrics.go`: concrete observers only.
- `internal/pluginruntime/plugin_api.go`: event contract and registration.
- `internal/pluginruntime/plugin_manager.go`: lifecycle and memory delivery.
- `internal/pluginruntime/plugin_queue.go`: optional SQLite delivery.
- `tests/plugins/`, `tests/pluginruntime/`: local unit tests, excluded from Git
  and Docker builds. Keep a separate backup; these tests are not in a fresh clone.

The executable imports `plugins` for registration. Implementations import the
runtime package; the runtime has no dependency on concrete plugins. Core bot
integration tests remain beside the root application files.

## Adding and enabling an observer

Add a Go file in `plugins/`, with `package plugins`. Register a factory from
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

For a personal implementation, use `plugins/local_<name>.go`, excluded by
`.gitignore`. Put its tests in `tests/plugins/local_<name>_test.go`; all of
`tests/` is ignored. Keep `plugins/` free of test and runtime files. For other
filenames, add their exact paths to `.git/info/exclude` before creating them. Verify with
`git check-ignore -v plugins/local_<name>.go`. Ignoring does not untrack committed
files or prevent `git add -f`; inspect staged changes before publishing and back
up private files separately. Docker includes `plugins/*.go`, even ignored files:
only publish images built from public source if personal code must stay private.

## Example

[`plugins/example.go`](../plugins/example.go) demonstrates registration, `Init`,
`Handle`, and `Close`. Enable it with `PLUGINS=example`, then recreate the service
with `docker compose up -d --build vk2tg`. Leave `PLUGINS` empty to disable it.

It logs lifecycle records and `example plugin received event` with
`event_version`, `text_length` (Unicode code points), and `attachments_count`.
It makes no network requests, creates no files, and uses the memory queue.
The local `tests/plugins/example_test.go` tests synthetic events,
safe JSON logs and cancellation without sending real Telegram messages.

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

Run `./scripts/test.sh test ./...`, `./scripts/test.sh test -race ./...`,
`./scripts/test.sh vet ./...`, and the normal `go build` command. The runner
copies Go source and local test files into a temporary workspace, placing tests
beside their packages as Go requires. It does not copy `.env`, production state
or `.git`, and removes its temporary files afterwards. No runtime API needs to
be exposed solely for testing. The ignored `tests/go.mod` prevents Go from
mistaking the stored test files for standalone packages.

`go test ./...` directly in the repository still runs core tests, but excludes
local plugin/runtime suites. The runner fails clearly if local suites are
missing. Tests use mock APIs and temporary databases. They cover memory delivery,
overflow, event isolation, lifecycle services, failures, optional persistence,
retries, restart recovery and exclusion from history/dry-run modes.
