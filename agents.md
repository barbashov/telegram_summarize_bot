# Telegram Summarize Bot

A Telegram bot that summarizes group chat messages using OpenRouter (free LLM API).

## Setup

1. Copy `.env.example` to `.env` and fill in your credentials:
   ```bash
   cp .env.example .env
   ```

2. Get your Telegram Bot Token from @BotFather
3. Get your OpenRouter API key from https://openrouter.ai
4. Get your Telegram User ID (send /start to @userinfobot)

## Commands

### Run Locally

```bash
# Install dependencies
go mod download

# Run the bot
go run main.go

# Or build and run
go build -o telegram_summarize_bot main.go
./telegram_summarize_bot
```

### Run with Docker

```bash
# Build the image
docker-compose build

# Run in background
docker-compose up -d

# View logs
docker-compose logs -f

# Stop
docker-compose down
```

### Development

```bash
# Lint
go vet ./...

# Test
go test ./...

# Format code
go fmt ./...
```

## Bot Setup in Telegram

1. Add bot to your group
2. Turn off "Group Privacy" for the bot (via @BotFather, /BotSettings)
3. Make bot an admin (or give it "Read Messages" permission)
4. Start the bot
5. Add the group ID to `ALLOWED_GROUPS` in your `.env`
6. Now any group member can use `@bot summarize`

## Available Commands

| Command | Description |
|---------|-------------|
| `@bot summarize` | Summarize messages from last 24h (any group member) |
| `@bot help` | Show available commands |

## Configuration

| Variable | Default | Description |
|----------|---------|-------------|
| `BOT_TOKEN` | (required) | Telegram Bot Token |
| `OPENROUTER_API_KEY` | (required) | OpenRouter API Key |
| `DB_PATH` | `./data/bot.db` | Path to SQLite database |
| `SUMMARY_HOURS` | `24` | Time window for summarization |
| `RETENTION_DAYS` | `7` | Message retention period |
| `MAX_MESSAGES` | `100` | Max messages to summarize |
| `RATE_LIMIT_SEC` | `60` | Rate limit between /summarize |
| `MODEL` | `meta-llama/llama-3.3-70b-instruct` | LLM model |
| `ALLOWED_GROUPS` | (required) | Comma-separated group IDs the bot operates in; empty = deny all |


## Features

- Time-based summarization (last 24 hours)
- Group allowlist (bot ignores non-configured groups)
- Rate limiting (1 request per minute)
- Automatic message cleanup (older than 7 days)
- Graceful shutdown
- Structured logging
- SQLite persistence (via Docker volume)

## Architecture

Telegram group chat summarizer bot written in Go. All bot UI text is in Russian.

**Data flow**: Telegram (long polling via telego) → `bot/handlers.go` stores messages in SQLite → `/summarize` fetches messages from DB → `summarizer/` sends them to OpenRouter API → response sent back to Telegram group.

### Key packages

- **`bot/`** — Telegram update handling (polling, not webhooks) and in-memory rate limiter. `Bot` struct owns the telego client, DB, summarizer, and rate limiter. Background goroutines handle message cleanup and rate limit entry cleanup.
- **`db/`** — SQLite via `github.com/glebarez/sqlite` (pure Go, no CGO). Two tables: `messages` (group_id, user_id, username, text, timestamp) and `last_summarize` (group_id, timestamp). Schema auto-migrates on startup.
- **`summarizer/`** — Uses `go-openai` client configured with OpenRouter base URL. Prompt is hardcoded in Russian.
- **`config/`** — Loads `.env` via godotenv. Required: `BOT_TOKEN`, `OPENROUTER_API_KEY`. All other settings have defaults.
- **`logger/`** — Thin wrapper around zerolog exposing package-level `Debug()`, `Info()`, `Warn()`, `Error()`, `Fatal()` functions.

### Rate limiting

In-memory (not persisted to DB). Keyed by `userID_groupID`. Cleanup runs every 5 minutes. Configurable via `RATE_LIMIT_SEC` (default 60s).

## Context

See [specs/initial.md](specs/initial.md) for project context and setup details.
