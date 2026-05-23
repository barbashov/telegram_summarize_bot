package admin

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"telegram_summarize_bot/fetcher"
	"telegram_summarize_bot/logger"
	"telegram_summarize_bot/summarizer"
	"telegram_summarize_bot/tgutil"

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
		a.deps.SendMessage(ctx, chatID, "Подождите "+formatDuration(remaining)+" перед следующим запросом.")
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

	result := fmt.Sprintf("🔗 *Суммаризация URL:*\n\n%s", summarizer.EscapeMarkdown(summary))
	chunks := splitMessage(result, 4096)
	if len(chunks) == 0 {
		return
	}
	a.deps.EditFormattedWithRetry(ctx, chatID, statusMsgID, chunks[0])
	for _, chunk := range chunks[1:] {
		a.deps.SendFormatted(ctx, chatID, chunk)
	}
}

func formatDuration(d time.Duration) string {
	seconds := int(d.Seconds())
	if seconds < 60 {
		return fmt.Sprintf("%d секунд", seconds)
	}
	minutes := seconds / 60
	return fmt.Sprintf("%d минут", minutes)
}

func splitMessage(text string, limit int) []string {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	if limit <= 0 || len(text) <= limit {
		return []string{text}
	}

	lines := strings.Split(text, "\n")
	var chunks []string
	var current strings.Builder

	appendChunk := func() {
		chunk := strings.TrimSpace(current.String())
		if chunk != "" {
			chunks = append(chunks, chunk)
		}
		current.Reset()
	}

	for _, line := range lines {
		line = strings.TrimRight(line, " ")
		candidateLen := len(line)
		if current.Len() > 0 {
			candidateLen = current.Len() + 1 + len(line)
		}

		if candidateLen <= limit {
			if current.Len() > 0 {
				current.WriteString("\n")
			}
			current.WriteString(line)
			continue
		}

		if current.Len() > 0 {
			appendChunk()
		}

		for len(line) > limit {
			chunks = append(chunks, strings.TrimSpace(line[:limit]))
			line = line[limit:]
		}
		if strings.TrimSpace(line) != "" {
			current.WriteString(line)
		}
	}

	appendChunk()
	return chunks
}
