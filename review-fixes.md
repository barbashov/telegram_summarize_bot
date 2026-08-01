# Review backlog — deferred fixes

Consolidated findings from the 2026-08-01 full-codebase review (5-agent internal
review + Codex `review.md`), minus the P0 items already fixed. Ordered by
priority. File/line references are as of the review date and may drift.

## P1 — correctness bugs with user-visible impact

### Scheduler fixes (`handlers/schedule.go`) — best done as one change
- **Revoked groups keep receiving summaries.** `GetEnabledSchedules` selects
  `enabled = 1` with no allowlist join, and `runScheduledSummary` never checks
  `IsGroupAllowed`. `/groups remove` therefore doesn't stop the daily digest.
  Fix: check `IsGroupAllowed` in `runScheduledSummary` (or join in the query).
- **`schedule now` cancels that day's digest.** The manual path shares
  `runScheduledSummary`, which unconditionally stamps `UpdateLastDailySummary`;
  the regular run then dedups against the same-day stamp and skips. Fix: don't
  update the checkpoint from the manual path.
- **Scheduled goroutines bypass concurrency and shutdown management.**
  `go b.runScheduledSummary(...)` runs outside `b.inflight`/`b.sem`: not drained
  on shutdown (can write to a closed DB), and groups sharing the default time
  fire an unbounded LLM/vision burst. Fix: wrap in `inflight` + semaphore.
- **Strict minute-equality can skip a day.** A dropped tick (slow DB, host
  suspend) around `HH:MM` means `s.Hour != now.Hour() || s.Minute != now.Minute()`
  never matches again that day. Fix: fire when "due today and not yet run today".

### Shutdown drain gap (`handlers/handlers.go:167`)
The `!ok` (updates channel closed) branch returns without `cancelPolling()` +
`drainHandlers()`. On SIGINT both select cases are ready and Go picks randomly,
so ~half of shutdowns skip the drain and in-flight handlers write to a closed
DB. Fix: mirror the ctx.Done branch.

### Cluster retry aborts on hallucinated output (`summarizer/summarizer.go:327`)
`sanitizeClusters` failure (1-based indexes, empty topics — common LLM
mistakes) returns immediately from inside the retry loop, while a JSON parse
failure retries 3×. Fix: treat it like a parse failure (log, set `lastErr`,
`continue`).

### Truncated summary responses never recover (`summarizer/summarizer.go:344`)
`SummarizeTopics` uses a fixed `finalMaxTokens = 1000` and never checks
`FinishReason == "length"`; with high `TOPIC_MAX` (config has no upper bound)
all 3 retries fail identically after 3 paid calls. Fix: mirror
`ClusterTopics`' budget bump, or scale the budget with `TopicMax`.

### Cross-process OAuth refresh race (`provider/tokenstore.go:185`)
The bot and CLIs (`usage`, `openai token-refresh`) each hold an independent
`TokenStore` over `openai_tokens.json`; `refreshLocked` refreshes from
in-memory state without re-reading the file or locking. A CLI refresh that
rotates the refresh token invalidates the bot's copy (`invalid_grant` until
re-auth); concurrent refreshes can trip reuse detection and kill the token
family. Fix: re-`Load()` from disk before refreshing (adopt newer tokens),
plus a file lock around refresh. Related smaller fixes:
- Fall back to the still-valid access token when refresh fails inside the
  5-minute buffer window (`tokenstore.go:167`).
- Guard `expires_in == 0` (currently yields an already-expired token → refresh
  POST on every LLM call) and tolerate string-typed `expires_in`
  (`tokenstore.go:32`) — same shape-assumption class as the fixed object-form
  `error`.

### Silent misconfiguration of security lists (`config/config.go:287`)
`parseIDList` drops malformed `ALLOWED_GROUPS`/`ADMIN_USER_IDS` entries with no
log; `ALLOWED_GROUPS` (documented required) is never validated non-empty. A
typo yields a bot that starts cleanly but serves nothing / locks out the
admin. Fix: warn (or fail startup) on unparseable entries and empty allowlist.
Also `envIntOr` (`config.go:278`): invalid/zero/negative values silently fall
back (`RETENTION_DAYS=0` intending "forever" silently becomes 7 days) — warn.

### Fetcher transport hygiene (`fetcher/fetcher.go:151`)
A fresh `http.Transport` per fetch with no `IdleConnTimeout` and no
`CloseIdleConnections()` leaks a socket + two goroutines per fetch for as long
as the server holds the connection. Fix: `DisableKeepAlives: true` (or
`IdleConnTimeout` + `defer transport.CloseIdleConnections()`). Also pass `ctx`
into DNS resolution: `net.DefaultResolver.LookupHost(ctx, host)` instead of
context-less `net.LookupHost` (`fetcher.go:111`), so a stalled resolver
doesn't outlive the request timeout.

## P2 — robustness and reporting accuracy

- **`/status` counters count attempts, duplicates, and failures**
  (`handlers/loops.go:57`, `db/db.go:442`, `summarizer.go:337`). Latency events
  are deferred unconditionally (fires on failures too; `db_add` fires on
  deduplicated INSERT OR IGNOREs), while error rows are per-retry-attempt. One
  fully-failed call reports "ОК: 1, ошибок: 3"; the 20% fail-ratio alarm
  computes on mismatched units. Fix: persist explicit outcome events.
- **`/usage` swallows DB errors** (`usage/usage.go:57`): all store errors
  dropped with `_` → a locked DB renders as a confident "Нет данных". Fix:
  log + surface "данные недоступны".
- **Stale quota can masquerade as live** (`usage/quota.go:71`): if the tier-3
  probe succeeds but the capture transport persisted nothing, the reload
  returns the already-stale snapshot labeled `SourceLive`, suppressing the
  staleness footer. Fix: only return `SourceLive` if `CapturedAt` advanced.
- **Edited-message ordering race** (`handlers/handlers.go:180`,
  `edited.go:34`): parallel dispatch means an edit can run before its original
  insert (systematic during backlog replay) — the edit no-ops and pre-edit
  text is stored. Fix: serialize updates per chat, or retry the edit once
  shortly after a miss.
- **Rate limiter release race** (`handlers/ratelimit.go:45`): `Release`
  deletes unconditionally, so a failed slow request can free a newer request's
  entry. Fix: store the grant timestamp and delete only if unchanged.
- **Multi-chunk summaries are fire-and-forget** (`handlers/summarize.go`,
  `sendSummary`): chunks after the first are sent best-effort; a failure loses
  the tail with the checkpoint already committed. Consider confirming all
  chunks before committing, or noting the failure in chat.
- **Provider polish**: handle `response.failed`/`response.incomplete` terminal
  events in the streaming loop (`provider/responses.go:141`); compute
  `total = prompt + completion` when `total_tokens` is absent
  (`provider.go:209`); use the configured `OAUTH_CODEX_VERSION` in
  `FetchWhamUsage` instead of the hardcoded const (`oauth.go:106`); treat
  unparseable `used-percent` as unknown rather than 0% used
  (`provider.go:268`).
- **Admin/UX batch**: `/groups remove` requires the group in `known_groups`,
  making revocation impossible when it never got there (`admin/groups.go:76`);
  pending-instructions state never expires and swallows later unrelated
  messages, including URLs (`admin/instructions.go:147`) — add a TTL; raw
  English `err.Error()` concatenated into Russian chat messages
  (`admin/url.go:38`, `admin/instructions.go:164`) — generic Russian text in
  chat, details to the log; hardcoded "24 часа" in the empty-window message
  and over-reported window when clamped to `lastSummarize`
  (`summarize.go:56,77`); schemeless `url` entities (bare domains) fail with
  "unsupported scheme" — prepend `https://` in `tgutil.ExtractURLs`
  (`tgutil/urls.go:64`).
- **DB hardening**: DSN param `_foreign_keys=on` is ignored by glebarez — use
  `?_pragma=foreign_keys(1)` so FK enforcement survives connection recycling
  (`db/db.go:167`); chmod the `-wal`/`-shm` sidecars too (`db.go:207`);
  row-scan errors swallowed with bare `continue` in `QueryBotEvents`,
  `QueryErrorCounts`, `scanGroups` — log them (`db.go:1057`, `db.go:1090`,
  `token_usage.go:85`).

## P3 — nice to have

- `prune-deleted`: hard-fail on an export without a chat `id` unless an
  explicit `--skip-chat-check` flag is passed (`cmd/prune_deleted.go:115`) —
  currently a stderr warning, and `--apply` against the wrong chat's export
  mass-deletes live rows.
- Time-window the recent-errors ring check so 5 lifetime errors stop flagging
  `/status` red forever (`metrics/metrics.go:376`).
- `abbrev` rounding boundary: 999,950+ renders "1000k" instead of "1.0M"
  (`usage/format.go:133`).
- Container healthcheck (Dockerfile `HEALTHCHECK` or compose `healthcheck:`,
  e.g. heartbeat file + `test`) so a wedged poller gets recycled —
  `restart: unless-stopped` only fires on exit.
- Don't let the deprecated `OPENROUTER_API_KEY` fallback satisfy `LLM_TOKEN`
  for `LLM_MODE=responses` (cross-wires an OpenRouter key to api.openai.com,
  `config/config.go:86`).
- SSRF dialer port allowlist (80/443) — currently any port on any public IP is
  dialable, usable as a blind port prober (`fetcher/fetcher.go:166`).
- `recover()` wrapper around the readability/dateparse parsing path —
  `araddon/dateparse` is abandoned upstream and parses attacker-controlled
  page metadata; a panic there kills the process.
- Boolean env spellings are inconsistent (`REPLY_THREADS` accepts only
  `false`/`0`; `VISION_STEERING` also accepts `no`/`off`) — unify.
