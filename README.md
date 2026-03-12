# Telegram Summarize Bot

Telegram bot that summarizes group chat messages using OpenRouter (OpenAI-compatible LLM API).

## Features

- Summarizes group chat messages from a configurable time window (default: last 24 hours)
- Per-group and global admin whitelist
- Rate limiting (1 request per minute per user per group)
- Automatic message cleanup (configurable retention period)
- SQLite persistence
- Graceful shutdown

## Setup

1. Copy `.env.example` to `.env` and fill in your credentials:
   ```bash
   cp .env.example .env
   ```

2. Get your **Telegram Bot Token** from [@BotFather](https://t.me/BotFather)
3. Get your **OpenRouter API key** from [openrouter.ai](https://openrouter.ai)
4. (Optional) Set `INITIAL_ADMINS` to a comma-separated list of Telegram user IDs for global admin access

## Running

### Locally

```bash
go run main.go
```

### Docker

```bash
docker-compose up -d
```

### From GHCR

```bash
docker pull ghcr.io/barbashov/telegram_summarize_bot:main
```

## Telegram Bot Setup

1. Add the bot to your group
2. Disable **Group Privacy** for the bot via [@BotFather](https://t.me/BotFather) → Bot Settings
3. Give the bot admin permissions (or at least "Read Messages")
4. Global admins from `INITIAL_ADMINS` are auto-added on startup

## Bot Commands

| Command | Description |
|---------|-------------|
| `/summarize` | Summarize messages from the configured time window (admins only) |
| `/addadmin <user_id>` | Add a user as admin for this group (admins only) |
| `/removeadmin <user_id>` | Remove an admin from this group (admins only) |
| `/listadmins` | List all admins in the current group (admins only) |

## Configuration

All configuration is via environment variables (`.env` file):

| Variable | Default | Description |
|----------|---------|-------------|
| `BOT_TOKEN` | *(required)* | Telegram Bot Token |
| `OPENROUTER_API_KEY` | *(required)* | OpenRouter API Key |
| `DB_PATH` | `./data/bot.db` | Path to SQLite database |
| `SUMMARY_HOURS` | `24` | Time window for summarization (hours) |
| `RETENTION_DAYS` | `7` | Message retention period (days) |
| `MAX_MESSAGES` | `100` | Max messages to include in summary |
| `RATE_LIMIT_SEC` | `60` | Cooldown between `/summarize` calls (seconds) |
| `MODEL` | `meta-llama/llama-3.3-70b-instruct` | LLM model via OpenRouter |
| `OPENROUTER_URL` | `https://openrouter.ai/api/v1` | OpenRouter API base URL |
| `INITIAL_ADMINS` | *(empty)* | Comma-separated Telegram user IDs for global admin access |
