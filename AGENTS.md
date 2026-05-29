# Telegram Summarize Bot

A Telegram bot that summarizes group chat messages using LLM APIs (OpenRouter, OpenAI, or OpenAI Codex subscription). All bot-facing text is Russian.

> 📖 **User-facing docs live in `README.md`** — setup, the full command list, every config variable, and feature descriptions. This file is the engineering/agent reference: build & test commands, architecture, and conventions. Keep README authoritative; don't duplicate it here.

## Commands

> ⛔️ **NEVER run the bot yourself**, even for a quick test: production shares the same bot token, so a local instance steals updates from prod. Verify with `make test`/`make lint` instead.

All build and quality gates go through the Makefile:

```bash
make build       # go build (binary: ./telegram_summarize_bot)
make test        # go test ./...
make lint        # golangci-lint — runs in Docker; already includes govet (don't run `go vet` separately)
make fmt         # gofmt
make vulncheck   # govulncheck (dependency/stdlib CVEs)
make gosec       # gosec static analysis
make security    # vulncheck + gosec

./telegram_summarize_bot openai auth   # OAuth login for the Codex subscription (LLM_MODE=oauth)
```

## Commands surface

- **Group** (mention the bot): `@bot summarize [hours]` (aliases `s`, `sub`); reply + `@bot [prompt]` to act on the replied-to message; `@bot help`.
- **Admin** (private chat, gated by `ADMIN_USER_IDS`): `/status`, `/reset`, `/groups`, `/instructions`, `/usage`, `/help`, and URL summarization.
- **CLI**: `openai auth`, `usage` (same report as `/usage`).

See README for the full behavior, arguments, and configuration (`config/config.go` is the source of truth; required: `BOT_TOKEN`, `ALLOWED_GROUPS`).

## Architecture

**Data flow**: Telegram (long polling via telego) → `handlers/update.go` stores messages in SQLite → `summarize` fetches messages from DB → `summarizer/` sends them to LLM API via `provider/` → response sent back to Telegram group.

### Key packages

- **`provider/`** — LLM provider abstraction. `LLMClient` interface with `Complete()` method. Three implementations: `completions.go` (go-openai for Chat Completions API), `responses.go` (openai-go SDK for Responses API), `oauth.go` (OAuth token injection wrapping responses client). `tokenstore.go` handles OAuth token persistence and auto-refresh. `CompletionResponse` carries `TokenUsage`; a `Recorder` (implemented by `*db.DB`) records per-call usage, and in OAuth mode a capture transport parses Codex `x-codex-*` quota headers into `RateLimitSnapshot` and persists them. `ParseCodexRateLimits` / `FetchWhamUsage` support the `/usage` quota resolver.
- **`handlers/`** — Telegram update handling (polling, not webhooks), rate limiter, and group commands: `summarize` (with `s`/`sub` shorthands), reply-summarization (`summarize_reply.go`), scheduled summaries, and help. `Bot` struct owns the telego client, DB, summarizer, and rate limiter. Background goroutines handle message cleanup, rate-limit-entry cleanup, stats cache refresh, and scheduled summaries. Outgoing text is rendered via `telegramify` (Markdown → MarkdownV2); admin reports are sent as plain text.
- **`handlers/admin/`** — Admin-only private chat commands (`/status`, `/reset`, `/groups`, `/instructions`, `/usage`, URL summarization). Decoupled from the parent via a `Deps` interface for messaging primitives.
- **`summarizer/`** — API-agnostic summarizer using `provider.LLMClient`. Prompt is hardcoded in Russian. Two-step process: cluster messages into topics, then summarize each topic. Each call sets `CompletionRequest.Operation` (cluster/summarize/text/url/vision) for per-operation usage accounting.
- **`usage/`** — Shared `/usage` report: aggregates `token_usage` into windows + per-model/per-operation breakdowns, resolves the Codex quota (cached → wham → live throwaway probe), and formats a plain-text Russian report. Used by both `handlers/admin` and the `cmd usage` CLI.
- **`cmd/`** — CLI subcommands. `auth.go` implements OAuth PKCE flow for OpenAI Codex subscription. `usage.go` prints the usage report from the CLI.
- **`db/`** — SQLite via `github.com/glebarez/sqlite` (pure Go, no CGO). Tables: `messages`, `last_summarize`, `allowed_groups`, `known_groups`, `group_schedules`, `bot_events`, `bot_error_log`, `bot_config`, `token_usage`. Schema auto-migrates on startup. The latest Codex quota snapshot is stored in `bot_config` under `codex_rate_limits`.
- **`config/`** — Loads `.env` via godotenv. Required: `BOT_TOKEN`. LLM config via `LLM_MODE`, `LLM_TOKEN`, `LLM_ENDPOINT`. Legacy `OPENROUTER_*` vars supported with deprecation warnings.
- **`logger/`** — Thin wrapper around zerolog exposing package-level `Debug()`, `Info()`, `Warn()`, `Error()`, `Fatal()` functions.

### Rate limiting

In-memory (not persisted to DB). Keyed by group ID (one summarize per group per `RATE_LIMIT_SEC`). Cleanup runs every 5 minutes. Configurable via `RATE_LIMIT_SEC` (default 60s).

## Conventions

- **All bot-facing text is Russian.** Keep new user-visible strings (commands, reports, errors) in Russian.
- **Tests are split per source file** — e.g. `help.go` → `help_test.go`, not one big test file per package.
- **Lint runs in Docker** via `make lint` (golangci-lint, which already includes `govet`). Don't run `go vet` separately or assume a local `golangci-lint` binary exists.
- Go `1.26+`; module path is `telegram_summarize_bot` (no `go.mod` replace tricks — internal imports use that prefix).

## Agent Workflow Rule

After any successful `git push` to this repository, include the exact GitHub Actions run URL triggered by that push in the final response.
Do not print the generic workflow file link.
If the exact run URL cannot be determined, say that explicitly instead of printing a fallback workflow link.

After code changes, update `README.md` if the changes affect user-visible behavior, commands, configuration, setup, architecture notes, or documented defaults.
Do not leave `README.md` stale when implementation changes make existing documentation inaccurate.
