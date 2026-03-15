# Telegram Summarize Bot

Telegram bot that summarizes group chat messages using OpenRouter (OpenAI-compatible LLM API).

## Features

- Topic-based summaries with a short TL;DR plus per-topic breakdown
- Summarizes messages from a configurable time window (default: last 24 hours)
- Optional per-request override: `@bot summarize 12`
- Group allowlist (bot ignores non-configured groups)
- Rate limiting (1 request per minute per group)
- Forwarded messages are stored with original author attribution and never treated as commands
- Automatic message cleanup (configurable retention period)
- Optional startup/shutdown alerts to selected Telegram users
- `/status` command in private chat for alert users: runtime metrics, latency percentiles, error ring buffer
- SQLite persistence
- Graceful shutdown

## Setup

1. Copy `.env.example` to `.env` and fill in your credentials:
   ```bash
   cp .env.example .env
   ```
2. Get your **Telegram Bot Token** from [@BotFather](https://t.me/BotFather).
3. Get your **OpenRouter API key** from [openrouter.ai](https://openrouter.ai).
4. Set `ALLOWED_GROUPS` to a comma-separated list of Telegram group IDs the bot should operate in.
5. If you want lifecycle alerts, set `ALERT_USER_IDS` to Telegram user IDs that already started a private chat with the bot.

## Running

### Locally

```bash
go mod download
go run main.go
```

### Docker Compose

```bash
docker-compose build
docker-compose up -d
```

### From GHCR

```bash
docker pull ghcr.io/barbashov/telegram_summarize_bot:main
```

## Telegram Bot Setup

1. Add the bot to your group.
2. Disable **Group Privacy** for the bot via [@BotFather](https://t.me/BotFather) -> Bot Settings.
3. Give the bot admin permissions, or at least permission to read messages.
4. Add the group ID to `ALLOWED_GROUPS` in your `.env`.
5. Mention the bot in group messages, for example `@your_bot summarize`.

If someone opens a private chat with the bot, it replies with setup guidance instead of handling commands there.

### Private Commands (alert users only)

Users listed in `ALERT_USER_IDS` can send `/status` in a private chat with the bot to get a runtime health report:

- Uptime
- Telegram API, LLM, and DB latency (min / mean / p95 / max) with traffic-light indicators
- Message and summarization counters
- Rate-limit hit count
- Error counts by type
- Recent error ring buffer (last 10 entries)

Any user not in `ALERT_USER_IDS` receives "Нет доступа."

## Bot Commands

Commands are triggered by mentioning the bot in a group message:

| Command | Description |
|---------|-------------|
| `@bot summarize [hours]` | Summarize messages from the last N hours. If the group was summarized more recently, only newer messages are included. |
| `@bot s [hours]` | Shorthand for `summarize` |
| `@bot sub [hours]` | Additional shorthand for `summarize` |
| `@bot help` | Show available commands |

## Configuration

All configuration is via environment variables (`.env` file):

| Variable | Default | Description |
|----------|---------|-------------|
| `BOT_TOKEN` | *(required)* | Telegram Bot Token |
| `OPENROUTER_API_KEY` | *(required)* | OpenRouter API Key |
| `ALLOWED_GROUPS` | *(optional but effectively required)* | Comma-separated group IDs the bot operates in. Empty means all non-private chats are denied. |
| `ALERT_USER_IDS` | *(optional)* | Comma-separated Telegram user IDs for startup/shutdown alerts |
| `DB_PATH` | `./data/bot.db` | Path to SQLite database |
| `SUMMARY_HOURS` | `24` | Default time window for summarization (hours) |
| `RETENTION_DAYS` | `7` | Message retention period (days) |
| `MAX_MESSAGES` | `250` | Max messages to include in summary |
| `TOPIC_MAX` | `5` | Max number of topics in a summary |
| `RATE_LIMIT_SEC` | `60` | Cooldown between summarize calls per group (seconds) |
| `MODEL` | `meta-llama/llama-3.3-70b-instruct` | LLM model via OpenRouter |
| `OPENROUTER_URL` | `https://openrouter.ai/api/v1` | OpenRouter API base URL |

Note: Telegram bots can send private messages only to users who already started a chat with the bot.
