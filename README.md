# vk2tg

A standalone Go service for a one-way VK -> Telegram relay.
Repository: https://github.com/mammuthus/vk2tg

**Development-only.** The Python [vk2telegram](https://github.com/mammuthus/vk2telegram)
remains the production baseline. VK is read-only and Telegram is destination-only.
This first stage only validates configuration, logs startup, and waits for
SIGINT/SIGTERM. It does not contact VK, Telegram, or a database, even with
`DRY_RUN=false`. A read-only VK HTTP client is available but is not called by
the bootstrap. No forwarding, Long Poll loop, Docker, or metrics exist yet.

## VK Client

`NewVKClient(config, baseURL, timeout)` takes the access token from `Config` and
requires a positive HTTP timeout. An empty base URL selects
`https://api.vk.com/method`; tests use a local `httptest.Server`. The client pins
VK API version `5.199` and exposes only:

- `UsersGet(ctx, userIDs)` for `users.get`: ID, first name, last name.
- `GetLongPollServer(ctx)` for `messages.getLongPollServer`: server, key, ts.

The timestamp uses `json.Number` to accept numeric and numeric-string responses
without floating-point conversion. All three Long Poll fields must be present.
Requests carry context and send credentials in form-encoded POST bodies, never
in the URL. Redirects are not followed; response bodies are limited to 1 MiB.
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
and raw transport diagnostics are not included in returned errors. No retries,
workers, automatic CAPTCHA handling, or real API calls are run by tests.

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

Stop with Ctrl+C or SIGTERM. Logs contain no tokens or private IDs. Do not dump
the configuration, enable shell tracing around credentials, or run development
against production resources. Keep `DRY_RUN=true` in the local development file.