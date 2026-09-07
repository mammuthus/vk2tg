# vk2tg

A standalone Go service for a one-way VK -> Telegram relay.
Repository: https://github.com/mammuthus/vk2tg

**Development-only.** The Python [vk2telegram](https://github.com/mammuthus/vk2telegram)
remains the production baseline. VK is read-only and Telegram is destination-only.
This first stage only validates configuration, logs startup, and waits for
SIGINT/SIGTERM. It does not contact VK, Telegram, or a database, even with
`DRY_RUN=false`. No forwarding, HTTP clients, Docker, or metrics exist yet.

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
gofmt -w main.go config.go config_test.go
go test ./...
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