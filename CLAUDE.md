# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Build & Run Commands

```bash
# Run locally (requires .env with BOT_TOKEN and OPENROUTER_API_KEY)
go run main.go

# Build binary
go build -o telegram_summarize_bot .

# Run with Docker
docker-compose up -d

# Lint / Test / Format
go vet ./...
go test ./...
go fmt ./...
```

## Architecture

Telegram group chat summarizer bot written in Go. Collects messages from groups, stores them in SQLite, and on `/summarize` command sends them to an OpenRouter LLM for summarization. All bot UI text is in Russian.

**Data flow**: Telegram (long polling via telego) → `bot/handlers.go` stores messages in SQLite → `/summarize` fetches messages from DB → `summarizer/` sends them to OpenRouter API → response sent back to Telegram group.

### Key packages

- **`bot/`** — Telegram update handling (polling, not webhooks) and in-memory rate limiter. `Bot` struct owns the telego client, DB, summarizer, and rate limiter. Background goroutines handle message cleanup and rate limit entry cleanup.
- **`db/`** — SQLite via `github.com/glebarez/sqlite` (pure Go, no CGO). Two tables: `messages` (group_id, user_id, username, text, timestamp) and `admins` (group_id, user_id). Schema auto-migrates on startup.
- **`summarizer/`** — Uses `go-openai` client configured with OpenRouter base URL. Prompt is hardcoded in Russian.
- **`config/`** — Loads `.env` via godotenv. Required: `BOT_TOKEN`, `OPENROUTER_API_KEY`. All other settings have defaults.
- **`logger/`** — Thin wrapper around zerolog exposing package-level `Debug()`, `Info()`, `Warn()`, `Error()`, `Fatal()` functions.

### Admin system

Global admins (from `INITIAL_ADMINS` env var) are stored with `group_id=0` in the admins table and have access in all groups. Per-group admins are added via `/addadmin` and scoped to that group. `IsAdmin` checks both `group_id=0` and the specific group.

### Rate limiting

In-memory (not persisted to DB). Keyed by `userID_groupID`. Cleanup runs every 5 minutes. Configurable via `RATE_LIMIT_SEC` (default 60s).
