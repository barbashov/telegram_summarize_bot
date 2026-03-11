# Project Context

## Telegram Summarize Bot

**Purpose**: Telegram bot that summarizes group chat messages using OpenRouter (free LLM API).

## Stack
- **Language**: Go 1.21
- **Bot API**: github.com/mymmrac/telego (polling mode)
- **Database**: SQLite (github.com/glebarez/sqlite)
- **LLM**: OpenRouter API (meta-llama/llama-3.3-70b-instruct)
- **Logging**: github.com/rs/zerolog

## Project Structure
```
telegram_summarize_bot/
├── main.go              # Entry point
├── config/config.go    # .env loading
├── db/db.go           # SQLite operations
├── bot/handlers.go    # Telegram handlers
├── bot/ratelimit.go   # Rate limiting (1/min)
├── summarizer/summarizer.go  # OpenRouter client
├── logger/logger.go   # Structured logging
├── .env               # Secrets (gitignored)
├── .env.example       # Template
├── Dockerfile         # Multi-stage build
├── docker-compose.yml # Bot + volume for DB
└── agents.md          # Commands reference
```

## Features
- Time-based summarization (last 24h, configurable)
- Global admin whitelist (configured via `INITIAL_ADMINS` in .env)
- Per-group admin management via `/addadmin`, `/removeadmin`, `/listadmins`
- Rate limiting (1 request per minute per user)
- Automatic message cleanup (older than 7 days)
- Graceful shutdown
- SQLite persistence via Docker volume

## Commands
| Command | Description |
|---------|-------------|
| `/summarize` | Summarize messages from last 24h (admins only) |
| `/addadmin <user_id>` | Add user to admin list (admins only) |
| `/removeadmin <user_id>` | Remove user from admin list (admins only) |
| `/listadmins` | List all admins in current group |

## Configuration (.env)
- `BOT_TOKEN` - Telegram Bot Token
- `OPENROUTER_API_KEY` - OpenRouter API Key
- `INITIAL_ADMINS` - Comma-separated admin user IDs (global, applies to all groups)
- `SUMMARY_HOURS` - Time window for summarization (default: 24)
- `RETENTION_DAYS` - Message retention period (default: 7)
- `MAX_MESSAGES` - Max messages to summarize (default: 100)
- `RATE_LIMIT_SEC` - Rate limit between /summarize (default: 60)
- `MODEL` - LLM model (default: meta-llama/llama-3.3-70b-instruct)

## Bot Setup
1. Add bot to group
2. Turn off "Group Privacy" via @BotFather
3. Make bot admin (or give "Read Messages" permission)
4. Start bot - global admins from `INITIAL_ADMINS` are auto-added on startup

## To Run
```bash
# Local
go run main.go

# Docker
docker-compose up -d
```
