# Telegram Summarize Bot

A Telegram bot that summarizes group chat messages using LLM APIs (OpenRouter, OpenAI, or OpenAI Codex subscription).

## Setup

1. Copy `.env.example` to `.env` and fill in your credentials:
   ```bash
   cp .env.example .env
   ```

2. Get your Telegram Bot Token from @BotFather
3. Configure your LLM provider (see Configuration below)
4. Get your Telegram User ID (send /start to @userinfobot)

## Commands

### Build Locally

```bash
# Install dependencies
go mod download

# Build
go build -o telegram_summarize_bot main.go
```

### Development

**NEVER run the bot by youself for testing.**

```bash
# Lint
make lint

# Test
make test

# Format code
make fmt
```

### OAuth Authentication

```bash
# Authenticate with OpenAI Codex subscription
./telegram_summarize_bot openai auth
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
| `LLM_MODE` | `completions` | LLM backend: `completions`, `responses`, or `oauth` |
| `LLM_TOKEN` | (required for completions/responses) | API token for the LLM provider |
| `LLM_ENDPOINT` | (mode-dependent) | API endpoint |
| `MODEL` | `meta-llama/llama-3.3-70b-instruct` | LLM model |
| `DB_PATH` | `./data/bot.db` | Path to SQLite database |
| `SUMMARY_HOURS` | `24` | Time window for summarization |
| `RETENTION_DAYS` | `7` | Message retention period |
| `MAX_MESSAGES` | `250` | Max messages to summarize |
| `RATE_LIMIT_SEC` | `60` | Rate limit between /summarize |
| `ALLOWED_GROUPS` | (required) | Comma-separated group IDs the bot operates in; empty = deny all |
| `OAUTH_TOKEN_DIR` | `./data` | Directory for OAuth token storage |
| `OAUTH_CLIENT_ID` | (Codex CLI default) | OAuth client ID (override for custom OAuth apps) |


## Features

- Time-based summarization (last 24 hours)
- Multiple LLM backends (Completions API, Responses API, OAuth)
- Group allowlist (bot ignores non-configured groups)
- Rate limiting (1 request per minute)
- Automatic message cleanup (older than 7 days)
- Graceful shutdown
- Structured logging
- SQLite persistence (via Docker volume)

## Architecture

Telegram group chat summarizer bot written in Go. All bot UI text is in Russian.

**Data flow**: Telegram (long polling via telego) → `handlers/update.go` stores messages in SQLite → `summarize` fetches messages from DB → `summarizer/` sends them to LLM API via `provider/` → response sent back to Telegram group.

### Key packages

- **`provider/`** — LLM provider abstraction. `LLMClient` interface with `Complete()` method. Three implementations: `completions.go` (go-openai for Chat Completions API), `responses.go` (openai-go SDK for Responses API), `oauth.go` (OAuth token injection wrapping responses client). `tokenstore.go` handles OAuth token persistence and auto-refresh.
- **`handlers/`** — Telegram update handling (polling, not webhooks), rate limiter, and group commands (summarize, schedule, help). `Bot` struct owns the telego client, DB, summarizer, and rate limiter. Background goroutines handle message cleanup, rate limit entry cleanup, stats cache refresh, and scheduled summaries.
- **`handlers/admin/`** — Admin-only private chat commands (`/status`, `/reset`, `/groups`, URL summarization). Decoupled from the parent via a `Deps` interface for messaging primitives.
- **`summarizer/`** — API-agnostic summarizer using `provider.LLMClient`. Prompt is hardcoded in Russian. Two-step process: cluster messages into topics, then summarize each topic.
- **`cmd/`** — CLI subcommands. `auth.go` implements OAuth PKCE flow for OpenAI Codex subscription.
- **`db/`** — SQLite via `github.com/glebarez/sqlite` (pure Go, no CGO). Tables: `messages`, `last_summarize`, `allowed_groups`, `known_groups`, `group_schedules`, `bot_events`, `bot_error_log`, `bot_config`. Schema auto-migrates on startup.
- **`config/`** — Loads `.env` via godotenv. Required: `BOT_TOKEN`. LLM config via `LLM_MODE`, `LLM_TOKEN`, `LLM_ENDPOINT`. Legacy `OPENROUTER_*` vars supported with deprecation warnings.
- **`logger/`** — Thin wrapper around zerolog exposing package-level `Debug()`, `Info()`, `Warn()`, `Error()`, `Fatal()` functions.

### Rate limiting

In-memory (not persisted to DB). Keyed by `userID_groupID`. Cleanup runs every 5 minutes. Configurable via `RATE_LIMIT_SEC` (default 60s).

## Agent Workflow Rule

After any successful `git push` to this repository, include the exact GitHub Actions run URL triggered by that push in the final response.
Do not print the generic workflow file link.
If the exact run URL cannot be determined, say that explicitly instead of printing a fallback workflow link.

After code changes, update `README.md` if the changes affect user-visible behavior, commands, configuration, setup, architecture notes, or documented defaults.
Do not leave `README.md` stale when implementation changes make existing documentation inaccurate.
