# Runtime And Operations

## Configuration

The binary reads these variables through [config.go](../config.go). It does not
load `.env`; the shell or Compose must supply the environment. All four required
values are required even in dry-run mode. IDs are nonzero signed decimal int64s.

| Variable | Required | Default / purpose |
| --- | --- | --- |
| `VK_ACCESS_TOKEN` | Yes | VK user access token |
| `VK_TARGET_PEER_ID` | Yes | One source VK peer |
| `TELEGRAM_BOT_TOKEN` | Yes | Telegram bot token with permission to send to the destination |
| `TELEGRAM_TARGET_CHAT_ID` | Yes | One destination chat/channel ID |
| `VK_BLOCKED_SENDER_IDS` | No | Empty; comma-separated numeric VK sender IDs |
| `DRY_RUN` | No | `true`; set `false` for Telegram delivery |
| `DEBUG` | No | `false`; set `true` for structured processing traces |
| `STATE_DB_PATH` | No | `state/vk2tg.sqlite` |

Keep credentials private; use [.env.example](../.env.example) as the template.
Never enable shell tracing or publish resolved Compose environments. Dry-run
still opens SQLite, uses VK rate protection, enriches messages and looks up
senders. It does not download media, send to Telegram or save message mappings.
It is not an offline test mode.

## Message Processing

[relay.go](../relay.go) accepts both `Out=0` and `Out=1`. It rejects wrong-peer,
missing-ID/sender and blocked-sender messages; existing mappings suppress live
duplicates. Empty service events or messages with no normalized content are
skipped. Long Poll attachment/reply hints trigger full-message enrichment.

[render.go](../render.go), [media.go](../media.go) and [sticker.go](../sticker.go)
implement delivery:

- Text has a bold sender label; reposts add a source-linked label when possible.
  User-controlled HTML is escaped. Long text uses UTF-16-aware chunks with
  4096-unit message and 1024-unit initial caption limits.
- Photos use the largest available size; multiple photos use albums of up to
  10 items. Documents use `sendDocument`.
- Stickers prefer the largest valid VK `images` asset, falling back to
  `images_with_background`. Missing assets can trigger a `getById` lookup using
  VK API 5.131. Original bytes are uploaded individually with `sendPhoto`, not
  `sendSticker`, without local conversion/resizing. Telegram may flatten alpha.
- Wall reposts include available text/media and nested wall copy history, with
  a fallback label when no content can be extracted. Generated wall links use
  `https://vk.ru`; supplied links are preserved. Link attachments avoid URLs
  already present in the accumulated text.
- Replies target the saved canonical Telegram ID. Missing mappings result in
  an ordinary send; Telegram is allowed to send even if the reply target is gone.
- Unsupported attachment types become `[Unsupported attachment]`. Forwarded
  message trees are counted but not rendered; videos/audio have no native send
  pipeline. VK edits/deletions are not mirrored.

All media is downloaded before sending a message. A later multipart send failure
can still leave partial Telegram output without a mapping.

## SQLite And Delivery Guarantees

[storage.go](../storage.go) uses pure-Go `modernc.org/sqlite`, one connection and
a five-second busy timeout. Tables are initialized by the application:

| Table | State |
| --- | --- |
| `message_map` | VK message ID -> canonical Telegram message ID; first-write-wins |
| `vk_rate_state` | Flood cooldown deadline and escalation level |
| `history_migration` | Created by migration; identity, counters, offset/boundary, pending ID and pause deadline |

Live delivery and full migration save the first primary Telegram message ID
only after the complete VK message is delivered. Replies and duplicate checks
survive restarts. Use one database per source/destination; do not share it
between simultaneous delivery processes.

The live Long Poll cursor is memory-only. Successful batches advance it after
all events are handled. Protocol `failed=1` advances the cursor, `failed=2`
refreshes the key while preserving the cursor, and `failed=3` resumes from
current events. Downtime or cursor expiration can lose events. Telegram sends
and SQLite commits are not atomic: a crash after acceptance can leave an
unmapped send and cause duplication if replayed. There is no exactly-once
guarantee. [Migration](history.md) adds a pending-delivery guard, not a durable
live cursor. No metrics or health HTTP endpoint is implemented.

## Rate Protection

[vk.go](../vk.go) and [vk_rate.go](../vk_rate.go) share a central per-client
limiter across VK API methods, including history and enrichment. API attempts
are spaced at least 500 ms apart; Long Poll waits are separate. The default VK
API version is 5.199, except the sticker fallback described above.

- Code 6: up to five client retries, nominal waits 2/4/8/16/30 seconds with 10%
  jitter. Transport errors, HTTP 429 and 5xx also use bounded client backoff.
- Codes 9/29: persist a cooldown of 30 minutes, then 60 minutes, then two hours
  on repeated flood failures. A successful API call after expiry resets the
  escalation. Live mode may retry, but subsequent API calls wait for cooldown;
  history commands stop on 9/29 without additional probes.
- Authentication/CAPTCHA/validation/manual-action errors (5/14/17/25) are not
  solved automatically. They propagate and stop the process; Docker's restart
  policy can restart it but cannot repair the account or token.

[telegram_rate.go](../telegram_rate.go) serializes sends and retries. The first
send after idle is immediate; subsequent sends reserve one second per message,
including each album item. A group/supergroup send response changes pacing to
three seconds per message without a metadata probe. A 429 with `retry_after`
delays the whole client, with cancellation-aware waiting. Other send errors are
returned, not blindly retried. Limits are local to one client; they cannot
coordinate another process using the same token.

## Logging And Privacy

[main.go](../main.go) emits JSON logs through `log/slog`. `DEBUG=true` enables
metadata traces for filters, VK attempts, media, Telegram pacing/retries,
canonical IDs and mapping saves. [debug.go](../debug.go) correlates live records
with message/batch IDs, event index and cursors; failures expose stage, category
and available numeric API/HTTP codes. Debug does not alter delivery behavior.

Traces exclude message text, sender names, tokens, Long Poll keys, media URLs,
filenames and binary payloads. IDs/cursors remain sensitive metadata. Restrict
log access and disable debug when detailed traces are no longer needed.

## Docker Production

Verified on 2026-09-12: `vk2tg` is the primary production relay, running the Go
binary in live mode with healthy Long Poll cycles. Project/service `vk2tg`,
container `vk2tg-vk2tg-1`, image `vk2tg:local`, volume `vk2tg_state`. Effective
settings are `DRY_RUN=false`, `DEBUG=true`, `STATE_DB_PATH=/data/state.db`.

[compose.yaml](../compose.yaml) overrides `.env` for `DRY_RUN` and
`STATE_DB_PATH`. It uses `unless-stopped` and `json-file` logs with `max-size=10m`,
`max-file=3` (roughly 30 MiB per container, not a global Docker storage cap).
[Dockerfile](../Dockerfile) builds with Go 1.27.1 and runs as UID/GID 10001 in
Alpine 3.23. Compose adds a read-only root filesystem, writable `/data` volume,
512 MiB `/tmp` tmpfs, dropped capabilities and a 30-second stop grace period.
Docker service boot autostart is enabled on the current host; a manually stopped
container remains stopped under `unless-stopped` until explicitly started.

Read-only checks, from the repository directory:

```sh
docker compose -p vk2tg -f compose.yaml config --quiet
docker compose -p vk2tg -f compose.yaml ps
docker compose -p vk2tg -f compose.yaml logs --tail 50 vk2tg
```

Look for `vk long poll connected` and recent `vk long poll cycle complete`
records, not just a running container. Apply code/config changes only when a
production update is intended:

```sh
docker compose -p vk2tg -f compose.yaml build vk2tg
docker compose -p vk2tg -f compose.yaml up -d --no-deps --force-recreate vk2tg
```

Recreation applies `.env` changes; a plain restart does not. Preserve the SQLite
volume and protected backups. Never use `down -v` or global cleanup for updates.
Use isolated resources for development and stop live delivery before an
authorized history operation. The application never starts another service.
