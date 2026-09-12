# History Replay And Migration

Both commands read the configured VK peer with `messages.getHistory` and use
the current sender blocklist, rendering/media pipeline and rate protection.
Messages are eligible regardless of `Out`. Counts refer to source VK records,
not Telegram API calls: albums and long messages can require multiple sends.
Neither command runs Long Poll or automatically starts live production afterward.

| Behavior | `test-history --count N` | `migrate-history` |
| --- | --- | --- |
| Scope | Latest 1-100 records, default 3 | All accessible history observed during the run |
| Order | Oldest -> newest within the selected window | Oldest -> newest across pages |
| Pagination | None; offset 0, `rev=0`, result reversed | Offset pagination, `rev=1`, 50-record batches |
| Existing mappings | Do not suppress replay sends | Mapped records are skipped |
| New mappings | In-memory replay map only | Persistent canonical SQLite mappings |
| Replies | Replay map, then SQLite fallback | Persistent SQLite mappings |
| Resume | None; rerun can duplicate sends | Persistent checkpoint and pending-delivery guard |
| Dry-run | Respects `DRY_RUN` | Requires `DRY_RUN=false` |

## Latest-Message Replay

After configuring the private `.env` as in the [README](../README.md), inspect
a small window without Telegram delivery:

```sh
(
	set -a
	. ./.env
	set +a
	export DRY_RUN=true
	exec ./vk2tg test-history --count 15
)
```

To send an explicitly intended replay, use `DRY_RUN=false` instead. Do not use
this command to resume a migration or deduplicate previously sent history.
It does not write `message_map`, even after successful delivery. It still opens
SQLite and can persist VK cooldown state. Implementation: [history.go](../history.go).

## Full Migration Procedure

Only run this as an authorized transfer, never alongside live delivery. Keep the
same SQLite database, VK peer and Telegram destination throughout. The command
does not clear mappings or reset completed migration state. If the destination
was emptied, obsolete state needs an explicitly authorized reconciliation before
a new transfer. Preserve the volume, VK cooldown and a verified backup; there is
no CLI reset option. Do not delete source history while paging.

For the included Compose deployment, from the repository directory:

```sh
docker compose -p vk2tg -f compose.yaml stop vk2tg
umask 077
backup_dir="$HOME/backups/vk2tg-$(date -u +%Y%m%dT%H%M%SZ)"
mkdir -p "$backup_dir"
docker cp vk2tg-vk2tg-1:/data/. "$backup_dir/"
```

Verify the private backup before proceeding. Copying the entire stopped data
directory preserves SQLite sidecar files too; arbitrary copies of a running
database are not a reliable backup. Start the migration only after that check:

```sh
docker compose -p vk2tg -f compose.yaml run --rm --no-deps vk2tg migrate-history
```

An interruption or failure exits unsuccessfully. Inspect logs and pending state
before attempting resume; do not launch another replay to compensate. After a
successful `migration complete` (or confirmed completed state on rerun), restore
the normal live service explicitly:

```sh
docker compose -p vk2tg -f compose.yaml up -d --no-deps vk2tg
docker compose -p vk2tg -f compose.yaml logs --tail 50 vk2tg
```

## Pagination, Checkpoints And Failure Handling

[migration.go](../migration.go) requests 50 records initially, then 51 at the
previous offset minus one to validate a one-record overlap. The boundary ID must
match the last processed ID. Shrinking totals, changed boundaries or invalid
pages stop the run. This detects boundary shifts, not every possible source
mutation. Later pages include newly appended records they observe; there is no
extra final-page probe. Arrivals between the final page and live startup can be
missed because the Long Poll cursor is not durable.

[migration_state.go](../migration_state.go) stores source/destination identity,
total, offset, last ID, sent/skipped counters, batches, pending ID, completion and
the next allowed batch time. A pending marker is saved before sending. Every
fully delivered message receives its canonical mapping before the progress
checkpoint advances. Skipped or already-mapped records advance progress without
creating a new mapping. Replies use the mappings built so far.

Run the same command with the same state to resume. A pending ID with a saved
mapping is reconciled without sending again. A pending ID without a mapping
stops before further VK requests: Telegram may already contain all or part of
that message. Manually reconcile it; never blindly clear the marker or resend.
Telegram delivery and SQLite commits cannot be one atomic operation, so this is
not an exactly-once guarantee. A completed migration rerun is a no-op, not an
incremental history catch-up.

One VK client/name cache spans the batches. Code 6 uses the normal backoff;
9/29 persists cooldown and immediately ends the entire migration without further
probes. Between batches, wait at least 60 seconds. Each batch encountering one
or more Telegram 429 responses adds 60 seconds to subsequent batch pauses; each
`retry_after` is also honored during delivery. The pause deadline survives
restart. Do not bypass it with another process.

Batch logs include VK ID range, processed/sent/skipped/failed counts, elapsed
time, request/error/retry counters and next pause. Progress counts persist;
request counters are process-local and restart at zero on resume. A nonzero
failure count needs reconciliation, not an assumption that nothing was sent.
