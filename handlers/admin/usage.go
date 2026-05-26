package admin

import (
	"context"

	"telegram_summarize_bot/config"
	"telegram_summarize_bot/usage"
)

// handleUsage reports token-usage history and, in OAuth mode, the Codex account
// quota. Resolving the quota may make a live call, so a placeholder is shown
// first and then edited with the final report.
func (a *Admin) handleUsage(ctx context.Context, chatID int64) {
	var quota usage.QuotaResult
	if a.cfg.LLMMode == config.LLMModeOAuth && a.llm != nil {
		msgID := a.deps.SendMessage(ctx, chatID, "⏳ Собираю данные об использовании…")
		quota = usage.ResolveCodexQuota(ctx, a.db, a.llm, a.cfg.Model, a.cfg.CodexQuotaTTL())
		report := usage.Build(ctx, a.db, a.cfg.Model, a.cfg.ModelContextTokens, quota)
		if msgID != 0 {
			a.deps.EditWithRetry(ctx, chatID, msgID, report.Format())
			return
		}
		a.deps.SendMessage(ctx, chatID, report.Format())
		return
	}

	report := usage.Build(ctx, a.db, a.cfg.Model, a.cfg.ModelContextTokens, quota)
	a.deps.SendMessage(ctx, chatID, report.Format())
}
