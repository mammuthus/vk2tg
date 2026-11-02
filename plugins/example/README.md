# Example Observer

[example.go](example.go) demonstrates registration, `Init`, `Handle`, and `Close`.
It uses the default in-memory queue, makes no network requests, creates no files
and exposes no HTTP endpoint, so it needs no Compose overlay.

Add `example` to the existing `PLUGINS` list in the private environment file.
An empty list disables all observers. Apply environment changes only during an
authorized update, preserving the deployment's complete Compose overlay list.

The plugin logs lifecycle records and `example plugin received event` with
`event_version`, `text_length` (Unicode code points), and `attachments_count`.
It does not log message text, identifiers, filenames or URLs.

[example_test.go](example_test.go) checks registration, lifecycle, safe logs and
cancellation using synthetic events. Run `go test ./plugins/example` from the
repository root, or `go test ./...` for the whole project.

See [the observer API](../../docs/plugins.md) for delivery guarantees and how to
implement a plugin.
