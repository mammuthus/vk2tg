# vk2tg

`vk2tg` is a small one-way relay from one VK chat to one Telegram channel,
written in Go. VK access is read-only; Telegram is used only for outbound
delivery.

## Features

- VK User Long Poll for new messages, including the token account's own messages
- One configured VK peer to one Telegram chat or channel
- Text messages, photos, photo albums, documents, and stickers
- Wall reposts with a linked source label when a source URL is available
- Link attachments without duplicate URLs
- Telegram replies for mapped VK reply relationships
- Sender blocklist
- SQLite message mapping for duplicate suppression and replies
- Manual historical replay

## Quick Start

Use Go 1.27.1 or newer. Copy the safe template and set the required values:

```sh
cp .env.example .env
chmod 600 .env
```

Build and run in dry-run mode first. `DRY_RUN` defaults to `true`:

```sh
go build
(
  set -a
  . ./.env
  set +a
  exec ./vk2tg
)
```

Set `DRY_RUN=false` only when delivery to Telegram is intended. The normal
command starts Long Poll for new messages; it does not replay history.

Run local checks with:

```sh
gofmt -w *.go
go test ./...
go test -race ./...
go vet ./...
go build
```

## Configuration

The service reads environment variables. It does not load `.env` itself; the
shell or Compose file supplies them.

| Variable | Required | Example / purpose |
| --- | --- | --- |
| `VK_ACCESS_TOKEN` | Yes | `replace-with-vk-user-token` |
| `VK_TARGET_PEER_ID` | Yes | `2000000123` |
| `TELEGRAM_BOT_TOKEN` | Yes | `replace-with-telegram-bot-token` |
| `TELEGRAM_TARGET_CHAT_ID` | Yes | `-1001234567890` |
| `VK_BLOCKED_SENDER_IDS` | No | Comma-separated VK sender IDs; empty by default |
| `DRY_RUN` | No | `true` by default; use `false` to send to Telegram |
| `DEBUG` | No | `false` by default; set `true` for metadata-only processing traces |
| `STATE_DB_PATH` | No | `state/vk2tg.sqlite` by default |

Keep real credentials in a private `.env` file. Do not enable shell tracing
when loading it.

## Debug Logging

Set `DEBUG=true` in the runtime environment (the local `.env` for Compose),
then recreate only the `vk2tg` service to apply it. `DEBUG=false` preserves
the compact operational logs. Debug does not change filters, retries, or
delivery behavior; use `DRY_RUN=true` separately to suppress Telegram delivery.
Dry-run still performs accepted-message enrichment and sender lookups, but
does not download media, send to Telegram, or save message mappings.

Debug JSON records carry `vk_message_id`, `batch_id`, `event_index`,
`ts_before`, and `ts_after` through live processing. They report parsing,
attachment/reply hints, each reached filter predicate, exact skip reasons,
VK API attempts, normalization counts, reply lookup, media downloads,
Telegram method/pacing/response/retry, canonical IDs, and mapping results.
Reply target IDs become available after full-message enrichment; raw Long Poll
reply hints are not dumped. Unknown attachment types are logged as `unknown`.
Errors expose stage, category and available numeric API/HTTP codes, not raw
error strings. Logs contain no message text, sender names, tokens, Long Poll
keys, media URLs, filenames, or binary payloads. IDs and cursors are still
sensitive metadata: restrict log access and disable debug after diagnosis.

`long poll batch complete` and `long poll ts advanced` occur only after all
events in a successful batch have been handled. Filtered events count as
handled. `failed_1` advances to the server-provided cursor without processing
updates; `failed_2` refreshes the key while preserving the cursor; `failed_3`
resumes from current events. Numeric cursors are logged, not persisted.

The live filter accepts target-chat messages regardless of `Out` (Long Poll
flags bit 2), including messages authored by the token's VK account. Outbox
remains visible in debug metadata but is not a skip reason. Wrong-peer,
sender-blocklist, and empty-service filters still apply.

## Historical Replay

Replay the latest 15 source records in chronological order with the same
rendering and delivery pipeline:

```sh
./vk2tg test-history --count 15
```

The command respects `DRY_RUN`. To send a deliberate replay, override it only
for that child process:

```sh
(
  set -a
  . ./.env
  set +a
  export DRY_RUN=false
  exec ./vk2tg test-history --count 15
)
```

Replay never writes normal persistent mappings. The requested count is source
VK records, so an album or long message can produce multiple Telegram calls.

## Full History Migration

Use `./vk2tg migrate-history` with `DRY_RUN=false` for a persistent, resumable
full transfer. Stop the live relay first and back up SQLite. Never run migration
and live delivery concurrently. The command does not clear existing mappings:
mapped messages are skipped. If the Telegram destination was emptied, only an
explicitly authorized reset of obsolete message mappings should precede a fresh
migration. Preserve VK cooldown state and the SQLite volume.

Migration calls `messages.getHistory` with `rev=1` and offset pagination,
processing up to 50 records oldest-first. A one-record overlap validates each
page boundary, including after resume. Shrinking history or a shifted boundary
stops the operation instead of silently skipping records. Do not delete source
history during migration. The first page logs the accessible count and estimated
batch count; newly appended records observed by subsequent pages are included.
The final page completes the observed history; messages arriving during the
handoff to live mode remain subject to the non-durable Long Poll cursor limit.

The normal filtering, enrichment, rendering, media, Telegram pacing and reply
pipeline is reused. One client/name cache spans all batches. No Long Poll server
request is made by the migration. Code 6 uses the existing VK client backoff;
VK 9/29 persists the cooldown and immediately terminates the transfer. There is
at least a 60-second pause before the next batch. Each batch that encounters a
Telegram 429 increases subsequent batch pauses by another 60 seconds; the
Telegram limiter also honors every `retry_after` within the batch. The next
allowed batch time is persisted, so restarting cannot bypass that pause.

`history_migration` stores the source/destination identity, offset, boundary ID,
counts, pending delivery ID and pause deadline. Every fully delivered VK message
gets a canonical `message_map` entry before its progress checkpoint is advanced.
Replies use those newly rebuilt persistent mappings. Skipped records advance
the progress checkpoint but do not create Telegram mappings. Batch logs include
VK ID range, sent/skipped/failed counts, VK/Telegram request/error/retry counters,
elapsed duration and the next pause.

Resume by running the same command with the same state, source and destination.
Completed mappings are not resent. A pending delivery with a persisted mapping
is recovered without sending again. A pending delivery without a mapping stops
for manual reconciliation: Telegram may have accepted it, or part of a multipart
message, before the process lost its response or SQLite commit. Bot API does not
provide an idempotency key, so automatic exactly-once recovery of that ambiguous
case cannot be guaranteed. Do not blindly remove a pending marker or reset
progress. A completed migration is a no-op on rerun. Restore normal live operation
only after the migration reports completion; an interrupted migration exits
unsuccessfully and must be resumed or reconciled first.

## Persistent State And Replies

SQLite stores the first successful Telegram message ID for each VK message.
The mapping suppresses duplicate normal events and lets a later VK reply become
a Telegram reply after restart. SQLite also preserves an active VK flood
cooldown so a process restart cannot bypass it. Use one `STATE_DB_PATH` per VK
peer and Telegram destination.

## Docker

The included Compose project starts the live relay with `DRY_RUN=false` and a
persistent SQLite volume at `/data/state.db`:

```sh
docker compose -p vk2tg -f compose.yaml build
docker compose -p vk2tg -f compose.yaml up -d --no-deps vk2tg
docker compose -p vk2tg -f compose.yaml logs --tail 50 vk2tg
```

It uses the `vk2tg` project namespace, the `vk2tg_state` volume, non-root
runtime user, `unless-stopped` restart policy, and rotated Docker logs. To
update only this service while retaining SQLite state:

```sh
docker compose -p vk2tg -f compose.yaml up -d --no-deps --force-recreate vk2tg
```

Do not use `down -v` when state must be retained.

## Notes And Limitations

- Unknown attachment types use `[Unsupported attachment]`.
- Empty service-only events are skipped; useful text or media is retained.
- Sticker images prefer VK's largest valid `images` variant, using
  `images_with_background` only if no unbacked image is available. They are
  uploaded individually via `sendPhoto`, with the existing caption/reply and
  without local conversion or resizing. Telegram's photo processing may flatten
  transparency. Ordinary photos still use the photo/album pipeline.
- Wall URLs generated by the relay use `https://vk.ru`; ready-made VK URLs are
  preserved.
- Long Poll has no durable cursor, and delivery is not exactly-once. Events can
  be missed during downtime; a crash after Telegram accepts a send can duplicate
  it later.
- Run one normal relay process per SQLite state database.

## Telegram Send Pacing

One client serves the configured destination. Its first send after idle is
immediate; subsequent requests are paced at one message per second, including
continuations and retries. Albums reserve one interval per item. Requests and
429 retries are serialized; `retry_after` pauses the whole Telegram client and
can be cancelled with the caller's context. VK rate protection is independent.

If a send response identifies the destination as a group/supergroup, pacing
changes to one message per three seconds (20/minute), without a metadata probe.
This follows the [Telegram limits FAQ](https://core.telegram.org/bots/faq#my-bot-is-hitting-limits-how-do-i-avoid-this).
Limits can vary, and other processes sharing the token are outside this local
limiter, so the 429 fallback remains necessary. Run history and live relay
separately when using the same destination/token.

[sendPhoto](https://core.telegram.org/bots/api#sendphoto) is used for VK sticker
images, not native `sendSticker`. Regression tests check PNG alpha, dimensions
and original bytes up to upload, not transparency after Telegram processing.
