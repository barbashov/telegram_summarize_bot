# Telegram Summarize Bot

Telegram bot that summarizes group chat messages using OpenRouter (OpenAI-compatible LLM API).

## Features

- Topic-based summaries with a short TL;DR plus per-topic breakdown
- Summarizes messages from a configurable time window (default: last 24 hours)
- Optional per-request override: `@bot summarize 12`
- **Daily scheduled summaries** — bot automatically posts a morning digest; configurable per group (`@bot schedule HH:MM`); admins can also trigger an immediate unscheduled summary with `@bot schedule now`
- Group allowlist (bot ignores non-configured groups)
- Rate limiting (1 request per minute per group)
- Forwarded messages are stored with original author attribution and never treated as commands
- Reply thread context in LLM prompts — reply-to relationships surface inline as `↩ a3f2b1c4: "quoted text"` (configurable via `REPLY_THREADS`)
- **Privacy-preserving storage** — no Telegram user IDs or usernames are stored; messages are attributed with an 8-char anonymous hash (HMAC-SHA256, group-scoped, non-reversible)
- Automatic message cleanup (configurable retention period)
- Optional startup/shutdown alerts to admin users
- **URL summarization** in admin private DMs — send a link, get a summary (with SSRF protection)
- Admin private commands (`/status`, `/groups`): runtime metrics and dynamic group management
- SQLite persistence
- Graceful shutdown

## Setup

1. Copy `.env.example` to `.env` and fill in your credentials:
   ```bash
   cp .env.example .env
   ```
2. Get your **Telegram Bot Token** from [@BotFather](https://t.me/BotFather).
3. Get your **OpenRouter API key** from [openrouter.ai](https://openrouter.ai).
4. Set `ALLOWED_GROUPS` to seed the initial group allowlist (stored in DB; can be managed at runtime via `/groups`).
5. If you want admin users (lifecycle alerts + group management), set `ADMIN_USER_IDS` to Telegram user IDs that already started a private chat with the bot.

## Running

### Locally

```bash
go mod download
go run main.go
```

### Docker Compose

```bash
docker-compose up -d
```

The compose file references the pre-built image from GHCR (`ghcr.io/barbashov/telegram_summarize_bot:main`) and includes a **Watchtower** sidecar that polls for new image digests every 60 seconds. Once Watchtower is running, every push to `main` automatically triggers a rolling restart on the server — no manual steps needed.

To check Watchtower activity:
```bash
docker logs watchtower
```

### From GHCR (manual pull)

```bash
docker pull ghcr.io/barbashov/telegram_summarize_bot:main
```

## Telegram Bot Setup

1. Add the bot to your group.
2. Disable **Group Privacy** for the bot via [@BotFather](https://t.me/BotFather) -> Bot Settings.
3. Give the bot admin permissions, or at least permission to read messages.
4. Either set `ALLOWED_GROUPS` in your `.env` (seeds the DB on first run), or use `/groups add <group_id>` in a private chat as an admin user.
5. Mention the bot in group messages, for example `@your_bot summarize`.

If someone opens a private chat with the bot, it replies with setup guidance instead of handling commands there.

### Private Commands (admin users only)

Users listed in `ADMIN_USER_IDS` can send the following commands in a private chat with the bot:

#### `/help`

Shows the list of available admin private commands.

#### `/status`

Runtime health report:

- Uptime
- Telegram API, LLM, and DB latency (min / mean / p95 / max) with traffic-light indicators
- Message and summarization counters
- Rate-limit hit count
- Error counts by type
- Recent error ring buffer (last 10 entries)

#### `/groups` — dynamic group management

| Command | Description |
|---------|-------------|
| `/groups` | List all known groups with ✅ (allowed) / ❌ (not allowed) status |
| `/groups add <group_id>` | Add group to the allowed list |
| `/groups remove <group_id>` | Remove group from the allowed list |

Groups become "known" when the bot is added to them or when a message is received from them. When the bot is added to a new group, all admin users are notified in private with the group name and the `/groups add` command to use.

The allowed-group list is stored in the database and is authoritative at runtime. `ALLOWED_GROUPS` in `.env` is used only to seed the database on first run (or after an upgrade from a version without this table).

#### URL summarization

Send a URL in a private message — the bot fetches the page, extracts the article text (using readability), and replies with a summary. Only admin users can use this feature; non-admins are ignored.

SSRF protection is built in: only `http`/`https` schemes are allowed, private/reserved IP ranges are blocked (including cloud metadata endpoints like `169.254.169.254`), DNS is pre-resolved and pinned to prevent rebinding, and redirect targets are re-validated.

Any user not in `ADMIN_USER_IDS` receives "Нет доступа."

## Bot Commands

Commands are triggered by mentioning the bot in a group message:

| Command | Description |
|---------|-------------|
| `@bot summarize [hours]` | Summarize messages from the last N hours. If the group was summarized more recently, only newer messages are included. |
| `@bot s [hours]` | Shorthand for `summarize` |
| `@bot sub [hours]` | Additional shorthand for `summarize` |
| `@bot schedule` | Show current daily summary schedule |
| `@bot schedule on` | Enable daily summary at the default time (admins only) |
| `@bot schedule off` | Disable daily summary (admins only) |
| `@bot schedule HH:MM` | Enable daily summary at the given UTC time, e.g. `08:00` (admins only) |
| `@bot schedule now` | Trigger an unscheduled summary immediately (admins only) |
| `@bot help` | Show available commands |

## Configuration

All configuration is via environment variables (`.env` file):

| Variable | Default | Description |
|----------|---------|-------------|
| `BOT_TOKEN` | *(required)* | Telegram Bot Token |
| `OPENROUTER_API_KEY` | *(required)* | OpenRouter API Key |
| `ALLOWED_GROUPS` | *(optional)* | Comma-separated group IDs used to seed the `allowed_groups` DB table on first run. Ignored on subsequent starts. |
| `ADMIN_USER_IDS` | *(optional)* | Comma-separated Telegram user IDs for admin users (alerts + `/groups` management). Falls back to `ALERT_USER_IDS` for backward compatibility. |
| `DB_PATH` | `./data/bot.db` | Path to SQLite database |
| `SUMMARY_HOURS` | `24` | Default time window for summarization (hours) |
| `RETENTION_DAYS` | `7` | Message retention period (days) |
| `MAX_MESSAGES` | `250` | Max messages to include in summary |
| `TOPIC_MAX` | `5` | Max number of topics in a summary |
| `RATE_LIMIT_SEC` | `60` | Cooldown between summarize calls per group (seconds) |
| `MODEL` | `meta-llama/llama-3.3-70b-instruct` | LLM model via OpenRouter |
| `OPENROUTER_URL` | `https://openrouter.ai/api/v1` | OpenRouter API base URL |
| `DAILY_SUMMARY_HOUR` | `7` | Default UTC hour for daily scheduled summaries (0–23) |
| `REPLY_THREADS` | `true` | Show reply context in summaries (`true`/`false`) |
| `URL_MAX_CHARS` | `64000` | Max extracted text chars for URL summarization |
| `ALL_PROXY` / `HTTPS_PROXY` | *(unset)* | Proxy URL for Telegram + OpenRouter traffic (`socks5://host:port`, `http://host:port`) |

Note: Telegram bots can send private messages only to users who already started a chat with the bot.
