# Telegram Summarize Bot

Telegram bot that summarizes group chat messages using OpenRouter (OpenAI-compatible LLM API).

## Features

- Summarizes group chat messages from a configurable time window (default: last 24 hours)
- Group allowlist (bot ignores non-configured groups)
- Rate limiting (1 request per minute per user per group)
- Forwarded messages attributed to original author, never treated as commands
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
4. Set `ALLOWED_GROUPS` to a comma-separated list of Telegram group IDs the bot should operate in

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
4. Add the group ID to `ALLOWED_GROUPS` in your `.env`

## Bot Commands

Commands are triggered by mentioning the bot in a group message:

| Command | Description |
|---------|-------------|
| `@bot summarize [hours]` | Summarize messages from the last N hours (default: 24) |
| `@bot s [hours]` | Shorthand for summarize |
| `@bot help` | Show available commands |

## Configuration

All configuration is via environment variables (`.env` file):

| Variable | Default | Description |
|----------|---------|-------------|
| `BOT_TOKEN` | *(required)* | Telegram Bot Token |
| `OPENROUTER_API_KEY` | *(required)* | OpenRouter API Key |
| `ALLOWED_GROUPS` | *(required)* | Comma-separated group IDs the bot operates in |
| `DB_PATH` | `./data/bot.db` | Path to SQLite database |
| `SUMMARY_HOURS` | `24` | Default time window for summarization (hours) |
| `RETENTION_DAYS` | `7` | Message retention period (days) |
| `MAX_MESSAGES` | `100` | Max messages to include in summary |
| `RATE_LIMIT_SEC` | `60` | Cooldown between summarize calls per user per group (seconds) |
| `MODEL` | `meta-llama/llama-3.3-70b-instruct` | LLM model via OpenRouter |
| `OPENROUTER_URL` | `https://openrouter.ai/api/v1` | OpenRouter API base URL |
