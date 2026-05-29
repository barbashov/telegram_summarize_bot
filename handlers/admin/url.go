package admin

import (
	"context"
	"errors"

	"telegram_summarize_bot/fetcher"
	"telegram_summarize_bot/logger"
	"telegram_summarize_bot/tgutil"

	telegramify "github.com/barbashov/telegramify-markdown-go"
	"github.com/mymmrac/telego"
)

// extractURL returns the first URL in the message, or "" if none. It delegates
// to tgutil.ExtractURLs for UTF-16-correct offset handling.
func extractURL(text string, entities []telego.MessageEntity) string {
	urls := tgutil.ExtractURLs(text, entities, 1)
	if len(urls) == 0 {
		return ""
	}
	return urls[0]
}

func (a *Admin) handleURLSummarize(ctx context.Context, chatID int64, rawURL string) {
	if !a.rateLimiter.Allow(chatID) {
		a.metrics.RateLimit.Record(0)
		remaining := a.rateLimiter.RemainingTime(chatID)
		a.deps.SendMessage(ctx, chatID, "Подождите "+tgutil.FormatDuration(remaining)+" перед следующим запросом.")
		return
	}

	statusMsgID := a.deps.SendMessage(ctx, chatID, "Загружаю страницу...")

	content, err := fetcher.Fetch(ctx, rawURL, a.cfg.URLMaxChars)
	if err != nil {
		logger.Error().Err(err).Str("url", rawURL).Msg("failed to fetch URL")
		msg := "Не удалось загрузить страницу: " + err.Error()
		if errors.Is(err, fetcher.ErrNoReadableContent) {
			msg = "Не удалось прочитать страницу — возможно, она требует входа или контент подгружается через JavaScript."
		}
		a.deps.EditWithRetry(ctx, chatID, statusMsgID, msg)
		return
	}

	if editErr := a.deps.EditMessage(ctx, chatID, statusMsgID, "Суммаризую содержимое..."); editErr != nil {
		logger.Warn().Err(editErr).Msg("failed to update status message")
	}

	summary, err := a.summarizer.SummarizeURL(ctx, rawURL, content, "")
	if err != nil {
		logger.Error().Err(err).Str("url", rawURL).Msg("failed to summarize URL")
		a.deps.EditWithRetry(ctx, chatID, statusMsgID, "Ошибка суммаризации. Попробуйте позже.")
		return
	}

	// The summary is Markdown; convert it (plus our header) to Telegram
	// MarkdownV2 so formatting renders instead of leaking as literal markers.
	rendered := telegramify.Markdownify("🔗 **Суммаризация URL:**\n\n" + summary)
	chunks := telegramify.Split(rendered, 4096)
	if len(chunks) == 0 {
		return
	}
	a.deps.EditFormattedWithRetry(ctx, chatID, statusMsgID, chunks[0])
	for _, chunk := range chunks[1:] {
		a.deps.SendFormatted(ctx, chatID, chunk)
	}
}
