# Review backlog — deferred fixes

Consolidated findings from the 2026-08-01 full-codebase review (5-agent internal
review + Codex `review.md`), minus the P0 items already fixed. Ordered by
priority. File/line references are as of the review date and may drift.

2026-08-02: all P1 items and the selected P2 batch (usage/quota reporting,
rate-limiter release race, DB hardening, edited-message ordering race) were
fixed and removed from this file; what remains is below.

## P2 — robustness and reporting accuracy

- **`/status` counters count attempts, duplicates, and failures**
  (`handlers/loops.go:57`, `db/db.go:442`, `summarizer.go:337`). Latency events
  are deferred unconditionally (fires on failures too; `db_add` fires on
  deduplicated INSERT OR IGNOREs), while error rows are per-retry-attempt. One
  fully-failed call reports "ОК: 1, ошибок: 3"; the 20% fail-ratio alarm
  computes on mismatched units. Fix: persist explicit outcome events.
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
- `openai-go` major upgrade: v2/v3 exist (currently on v1.12.0, latest v1).
  Breaking API migration for `provider/responses.go`; do deliberately, not as
  part of routine dependency bumps.
