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
5. From your personal chat, send `/addadmin <your_user_id>` to add yourself as admin
6. Now you can use `/summarize` in the group

## Available Commands

| Command | Description |
|---------|-------------|
| `/summarize` | Summarize messages from last 24h (admins only) |
| `/addadmin <user_id>` | Add user to admin list (admins only) |
| `/removeadmin <user_id>` | Remove user from admin list (admins only) |
| `/listadmins` | List all admins in current group |

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

## Features

- Time-based summarization (last 24 hours)
- Whitelist-based admin system (per-group)
- Rate limiting (1 request per minute)
- Automatic message cleanup (older than 7 days)
- Graceful shutdown
- Structured logging
- SQLite persistence (via Docker volume)

## Context

See [specs/initial.md](specs/initial.md) for project context and setup details.
