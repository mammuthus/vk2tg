# vk2tg

A standalone Go service for a one-way VK -> Telegram relay.
Repository: https://github.com/mammuthus/vk2tg

The Python [vk2telegram](https://github.com/mammuthus/vk2telegram) remains the
production baseline. This minimal relay can run alongside it with the same VK
account/peer and Telegram bot/channel. VK is strictly read-only; Telegram is
outbound-only. Neither relay consumes updates on behalf of the other, and this
service never marks messages as read. Running both intentionally produces two
deliveries of new messages; every message from this service ends with a separate
line: `отправлено через vk2tg`.

`DRY_RUN=true` is the default: VK Long Poll, filtering and normalization run, but
Telegram requests and media downloads do not. Explicit `DRY_RUN=false` enables
delivery. There is no database, metrics server, Telegram polling, or reverse relay.

## Relay Loop

The sequential loop acquires a fresh Long Poll cursor on startup and processes
only new message events (type 4, user Long Poll version 3, mode 2). It does not
load history. The event provides message ID at index 1, flags at 2, peer at 3,
text at 5, extra fields at 6 and attachment hints at 7. The outbox bit is `2`.
Sender ID comes from `extra.from`, or the peer for a direct inbound user message.
Unknown chat senders and service events are ignored. Peer, outbox and the sender
blocklist are checked before metadata requests or attachment processing.

Plain text events require no `messages.getById`. Attachment, forward or reply
hints trigger that read-only call to obtain the full text and attachments, and
the filters are applied again. Sender names use `users.get`, with a bounded
in-memory cache (up to 1024 entries); negative community senders use the neutral
label `VK community`. Forward/reply relationships are not reconstructed yet.

The cursor advances after the batch has been processed. `failed=1` replaces only
`ts`; `failed=2` refreshes server/key while retaining `ts`; `failed=3` obtains a
fresh cursor and logs a possible gap. Other failed codes stop the relay.

**Delivery limitations:** no durable cursor, queue or deduplication exists yet.
Restart and expired-cursor recovery can miss messages. A failure partway through
a multi-part delivery can leave a partial message; accepted sends with lost HTTP
responses are inherently ambiguous. There is no exactly-once guarantee or
automatic replay of previously sent parts. This is a parallel evaluation relay,
not a lossless replacement for the existing service.

## VK Client

The normal runtime uses only the methods below; manual history replay also uses
`messages.getHistory` as described in the next section.

`NewVKClient(config, baseURL, timeout)` takes the access token from `Config` and
requires a positive HTTP timeout. An empty base URL selects
`https://api.vk.com/method`; tests use a local `httptest.Server`. The client pins
VK API version `5.199` and exposes only:

- `UsersGet(ctx, userIDs)` for `users.get`: ID, first name, last name.
- `GetLongPollServer(ctx)` for `messages.getLongPollServer`: server, key, ts.
- `GetMessageByID(ctx, id)` for `messages.getById`: only needed message fields.
- `WaitLongPoll(ctx, server)` for read-only `a_check` waits with cursor updates.

The timestamp uses `json.Number` to accept numeric and numeric-string responses
without floating-point conversion. All three Long Poll fields must be present.
API requests carry context and send the access token in form-encoded POST bodies,
never in the URL. Long Poll uses its own key in the query, not the access token;
neither URL nor key is logged. Redirects are not followed; response bodies are
limited to 1 MiB. Runtime VK timeout is 35 seconds; Long Poll wait is 25 seconds.
Do not log the client, configuration, or Long Poll response (its key is secret).

Errors can be inspected with `errors.As` / `errors.Is`:

| Error | Meaning |
| --- | --- |
| `*VKTransportError` | Connection/read failure, cancellation or timeout; cancellation and deadlines preserve the context sentinel |
| `*VKHTTPError` | Non-200 HTTP status, available as `StatusCode` |
| `ErrVKInvalidJSON` | Malformed JSON, including trailing content |
| `ErrVKInvalidResponse` | Invalid envelope, payload, missing fields or oversized response |
| `*VKAPIError` | VK error with `Code`, safe `Message`, and `Kind()` |

VK codes are classified as 5 = authentication, 14 = CAPTCHA, 17 = validation,
25 = manual action; other codes are generic API errors. Messages are generated
locally from the code. Remote error messages, request parameters, response bodies,
and raw transport diagnostics are not included in returned errors. Tests make no
real API calls. No automatic CAPTCHA/validation handling exists.

## Text And Attachments

Ordinary text starts with `<b>Sender name</b>:`; wall reposts use
`<b>Sender name</b> (репост):`. Sender and user text are HTML-escaped and line
breaks are preserved. Wall text, a public `https://vk.com/wallOWNER_ID_POST_ID`
link when IDs exist, nested attachments and `copy_history` are retained. An
otherwise empty wall gets `📰 Запись на стене`; source labels are not fetched.
Recursive wall traversal is limited to eight levels. Unsupported attachment
types receive a plain placeholder rather than silently losing their presence.

Photos are downloaded at the largest available size and uploaded using
`sendPhoto`; consecutive photos are grouped with `sendMediaGroup` in batches of
up to 10 (a remaining single photo uses `sendPhoto`). Documents use `sendDocument`.
VK media URLs are never sent to Telegram as the media source. Downloads have a
60-second timeout and limits of 10 MB/photo and 50 MB/document. No transcoding or
oversize fallback is implemented; Telegram may reject unsupported media.

The first media item carries the text as its caption. Other album items and
subsequent media sends carry just the footer on a separate line. Long captions
use up to 1024 UTF-16 units; remaining text is sent as `sendMessage` continuation
parts of up to 4096 units. Every outgoing part has exactly one service-added
footer, including every album item. Its size and the first-part header are
reserved before splitting plain text, then HTML escaping is applied so entities
and formatting tags are never split. Names are capped at 256 UTF-16 units.

For each message with media, a private `vk2tg-media-*` directory is created under
the OS temporary directory. Downloads use `vk-media-*` files; replayable
multipart bodies use `telegram-upload-*` files. Files have mode 0600, directories
0700. All source media is downloaded before the first send; the whole directory
is removed on success, error or cancellation. A forced kill or machine crash
can leave files behind. Do not run broad temporary-file cleanup on a shared host.

## Errors And Retry

VK transport errors, HTTP 429/5xx and API codes 6/9/10 retry after two seconds.
Malformed Long Poll responses retry without advancing the cursor. VK 5/14/17/25
and other non-transient API errors stop the relay with a safe error. Media
network errors and HTTP 429/5xx retry before sending, with the same short pause.

The Telegram client uses direct HTTPS POSTs, JSON for text and disk-backed
multipart uploads for files, with a 120-second timeout and no redirects.
`TelegramError` exposes numeric HTTP/API codes, never the remote description.
HTTP/API 429 waits for `retry_after` (one second fallback) and retries the same
body from its beginning. All waits and HTTP calls are context-cancelable.
Other Telegram send errors stop the relay, without a service message in the
channel or automatic resend that could duplicate an already accepted delivery.

Logs contain only safe operational facts/counts, including `would relay message`
in dry-run mode; no body, sender, media URLs, tokens or private IDs are logged.

## Manual Historical Test

`./vk2tg test-history --count 3` selects the latest three records from the configured
peer using `messages.getHistory` (`offset=0`, `rev=0`). The newest-first response
is reversed and delivered oldest-first. Count defaults to 3 and is restricted to
1..100. This command does not acquire a Long Poll server or start the runtime loop.

It uses the same sender lookup/cache, normalization, wall renderer, HTML escaping,
footer, caption splitting, media downloads and Telegram upload pipeline as the
runtime. Full messages are already present in history, so there is no additional
`messages.getById` call. Reply/forward relationships are still not reconstructed;
unsupported attachment types retain the existing placeholder behavior.

This is an explicit manual replay: owner/outbox messages are eligible, unlike the
inbound-only Long Poll runtime. Peer checks and sender blocklist still apply before
metadata or media work. Blocked records are skipped, not replaced with older ones.
Fewer than N available/eligible records therefore means fewer than N sends. N counts
source VK records, not Telegram API calls: albums and continuation parts may produce
multiple Telegram messages. Every part retains the service footer.

The command respects `DRY_RUN`. To deliberately send using a trusted local `.env`,
without changing the file or the old relay:

```sh
(
  set -a
  . ./.env
  set +a
  export DRY_RUN=false
  exec ./vk2tg test-history --count 3
)
```

Logs report only selected/sent/skipped counts, chronological positions, media
counts, repost flags and successful Telegram method names. They never include
message content, private IDs, tokens or URLs. The command stops on send failure;
earlier records or parts may already have been delivered. Do not blindly rerun it:
there is no replay deduplication. Temporary media follows the runtime cleanup rules.

## Configuration

Configuration comes exclusively from environment variables. The application does
not parse `.env`. IDs are nonzero signed 64-bit decimal integers; negative chat
and community identifiers are supported. Invalid values fail startup without
printing their contents. Tokens are required but are not validated via network.

| Variable | Required | Meaning |
| --- | --- | --- |
| `VK_ACCESS_TOKEN` | Yes | VK user access token |
| `VK_TARGET_PEER_ID` | Yes | One source peer ID |
| `TELEGRAM_BOT_TOKEN` | Yes | Destination bot token |
| `TELEGRAM_TARGET_CHAT_ID` | Yes | One destination chat/channel ID |
| `VK_BLOCKED_SENDER_IDS` | No | Comma-separated sender IDs, deduplicated into a set; empty by default |
| `DRY_RUN` | No | Defaults to `true`; set `false` explicitly to disable |

`DRY_RUN` accepts Go's `strconv.ParseBool` values (`true`/`false`, `1`/`0`,
`t`/`f`, including the supported uppercase forms). Blank means the safe default.
Blocklists accept whitespace around IDs, but reject empty entries and zero.
Legacy `BOT_TOKEN` must be renamed to `TELEGRAM_BOT_TOKEN` when preparing the
local environment; there is no runtime fallback to the old name.

## Local Checks And Run

Use Go 1.27.1 or newer; no third-party dependencies are required.

```sh
gofmt -w *.go
go test ./...
go test -race ./...
go vet ./...
go build
```

Use `.env.example` as the fake-value template. Keep any real `.env` private
(`chmod 600 .env`); it is ignored by Git. For a trusted shell-compatible local
file only, run in a subshell so credentials do not remain in the parent shell:

```sh
(
  set -a
  . ./.env
  set +a
  exec ./vk2tg
)
```

Keep `DRY_RUN=true` for the initial run. To deliberately enable parallel delivery,
use the same command with `export DRY_RUN=false` immediately before `exec ./vk2tg`.
This only changes the new process environment, not the old service. Do not run
multiple copies of this new relay unless additional duplicate delivery is wanted.

Stop with Ctrl+C or SIGTERM. Do not dump configuration, enable shell tracing around
credentials, or log the Long Poll response. No changes to the old relay are needed.