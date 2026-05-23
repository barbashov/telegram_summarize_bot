package handlers

import (
	"context"
	"errors"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"telegram_summarize_bot/fetcher"
	"telegram_summarize_bot/logger"
	"telegram_summarize_bot/summarizer"
	"telegram_summarize_bot/tgutil"

	"github.com/mymmrac/telego"
)

// replyMaxLinks caps how many links the bot follows when summarizing a single
// replied-to message, bounding latency and cost.
const replyMaxLinks = 3

// replyPart is one condensed input to the unified summary: a per-link summary
// or an image description.
type replyPart struct {
	label string
	body  string
}

// handleSummarizeReply handles "@bot summarize" sent as a reply to another
// message. It acts on that one message — summarizing linked page(s), describing
// image(s), and/or summarizing the message text — and replies with a single
// unified summary. When several content kinds are present they are blended into
// one summary; a lone link or image short-circuits to its own output.
func (b *Bot) handleSummarizeReply(ctx context.Context, update telego.Update) {
	msg := update.Message
	groupID := msg.Chat.ID
	reply := msg.ReplyToMessage

	text, entities := replyTextAndEntities(reply)
	links := tgutil.ExtractURLs(text, entities, replyMaxLinks)
	photos := extractPhotoRecords(reply)
	// prose is the message text with the URL/anchor spans removed, so a bare
	// link ("https://…") leaves nothing and short-circuits to a plain link
	// summary, while genuine surrounding prose is still taken into account.
	prose := strings.TrimSpace(residualText(text, entities))

	hasOther := len(links) > 0 || len(photos) > 0
	// Plain text is only worth summarizing on its own above a threshold; when
	// there's also a link or image, the prose is folded in regardless of length.
	includeText := prose != "" && (utf8.RuneCountInString(prose) >= b.cfg.ReplyMinChars || hasOther)

	if len(links) == 0 && len(photos) == 0 && !includeText {
		switch {
		case hasUnsupportedMedia(reply):
			b.sendMessageReply(ctx, groupID, int64(reply.MessageID), "Этот тип сообщения пока не поддерживается для суммаризации.")
		case prose != "":
			b.sendMessageReply(ctx, groupID, int64(reply.MessageID), "Сообщение слишком короткое для суммаризации.")
		default:
			b.sendMessageReply(ctx, groupID, int64(reply.MessageID), "Нечего суммаризировать в этом сообщении.")
		}
		return
	}

	if !b.rateLimiter.Allow(groupID) {
		b.metrics.RateLimit.Record(0)
		remaining := b.rateLimiter.RemainingTime(groupID)
		b.sendMessageReply(ctx, groupID, int64(reply.MessageID), "Подождите "+formatDuration(remaining)+" перед следующим запросом суммаризации.")
		return
	}
	committed := false
	defer func() {
		if !committed {
			b.rateLimiter.Release(groupID)
		}
	}()

	statusMsgID := b.sendMessageReply(ctx, groupID, int64(reply.MessageID), "Обрабатываю сообщение...")

	instructions := b.loadGroupSummaryInstructions(ctx, groupID)

	// Condense each link and image into compact text. The message text stays
	// raw and is added at blend time.
	var parts []replyPart
	var lastLinkErr error
	for _, link := range links {
		content, ferr := b.fetchURL(ctx, link, b.cfg.URLMaxChars)
		if ferr != nil {
			lastLinkErr = ferr
			logger.Warn().Err(ferr).Str("url", link).Msg("reply-summarize: failed to fetch URL")
			continue
		}
		summary, serr := b.summarizer.SummarizeURL(ctx, link, content, instructions)
		if serr != nil {
			lastLinkErr = serr
			logger.Warn().Err(serr).Str("url", link).Msg("reply-summarize: failed to summarize URL")
			continue
		}
		if summary = strings.TrimSpace(summary); summary != "" {
			parts = append(parts, replyPart{label: "Ссылка " + link, body: summary})
		}
	}

	visionDisabled := false
	for _, photo := range photos {
		desc, derr := b.summarizer.DescribeImage(ctx, photo)
		if errors.Is(derr, summarizer.ErrVisionDisabled) {
			visionDisabled = true
			continue
		}
		if derr != nil {
			logger.Warn().Err(derr).Msg("reply-summarize: failed to describe image")
			continue
		}
		if desc = strings.TrimSpace(desc); desc != "" {
			parts = append(parts, replyPart{label: "Изображение", body: desc})
		}
	}

	var result string
	switch {
	case len(parts) == 0 && !includeText:
		// Nothing usable came back; explain why.
		switch {
		case len(links) > 0:
			if errors.Is(lastLinkErr, fetcher.ErrNoReadableContent) {
				b.editWithRetry(ctx, groupID, statusMsgID, "Не удалось прочитать страницу — возможно, она требует входа или контент подгружается через JavaScript.")
			} else {
				b.editWithRetry(ctx, groupID, statusMsgID, "Не удалось загрузить ссылку.")
			}
		case visionDisabled:
			b.editWithRetry(ctx, groupID, statusMsgID, "Распознавание изображений отключено.")
		default:
			b.editWithRetry(ctx, groupID, statusMsgID, "Не удалось обработать сообщение. Попробуйте позже.")
		}
		return
	case len(parts) == 1 && !includeText:
		// A lone link or image: its condensed output is the answer.
		result = parts[0].body
	default:
		// Text-only, or multiple parts → blend into one unified summary.
		summary, serr := b.summarizer.SummarizeText(ctx, buildReplyMaterial(parts, prose, includeText), instructions)
		if serr != nil {
			logger.Error().Err(serr).Msg("reply-summarize: failed to summarize")
			b.editWithRetry(ctx, groupID, statusMsgID, "Ошибка суммаризации. Попробуйте позже.")
			return
		}
		result = strings.TrimSpace(summary)
	}

	if result == "" {
		b.editWithRetry(ctx, groupID, statusMsgID, "Нет данных для суммаризации.")
		return
	}

	chunks := splitTelegramMessage("📝 *Суммаризация:*\n\n"+summarizer.EscapeMarkdown(result), telegramMessageLimit)
	if err := b.editFormattedFinal(ctx, groupID, statusMsgID, chunks[0]); err != nil {
		logger.Error().Err(err).Int64("chat_id", groupID).Msg("reply-summarize: failed to send result")
		return
	}
	for _, chunk := range chunks[1:] {
		b.sendFormatted(ctx, groupID, chunk)
	}
	committed = true
}

// buildReplyMaterial assembles the labeled blob fed to SummarizeText: each
// condensed part followed by the raw message text when included.
func buildReplyMaterial(parts []replyPart, text string, includeText bool) string {
	var sb strings.Builder
	for _, p := range parts {
		sb.WriteString(p.label)
		sb.WriteString(":\n")
		sb.WriteString(p.body)
		sb.WriteString("\n\n")
	}
	if includeText {
		sb.WriteString("Текст сообщения:\n")
		sb.WriteString(text)
		sb.WriteString("\n")
	}
	return strings.TrimSpace(sb.String())
}

// residualText returns the message text with its url and text_link entity spans
// removed, leaving the surrounding prose. Entity offsets/lengths are UTF-16 code
// units, so the text is sliced in UTF-16 space.
func residualText(text string, entities []telego.MessageEntity) string {
	units := utf16.Encode([]rune(text))
	remove := make([]bool, len(units))
	for _, e := range entities {
		if e.Type != "url" && e.Type != "text_link" {
			continue
		}
		start, end := e.Offset, e.Offset+e.Length
		if start < 0 || start > end || end > len(units) {
			continue
		}
		for i := start; i < end; i++ {
			remove[i] = true
		}
	}
	kept := make([]uint16, 0, len(units))
	for i, u := range units {
		if !remove[i] {
			kept = append(kept, u)
		}
	}
	return string(utf16.Decode(kept))
}

// replyTextAndEntities returns a message's text and its entities, preferring
// Text/Entities and falling back to Caption/CaptionEntities (media messages
// carry their text in the caption).
func replyTextAndEntities(msg *telego.Message) (string, []telego.MessageEntity) {
	if msg == nil {
		return "", nil
	}
	if msg.Text != "" {
		return msg.Text, msg.Entities
	}
	return msg.Caption, msg.CaptionEntities
}

// hasUnsupportedMedia reports whether the message carries a media type the
// reply-summarizer can't handle yet (everything other than images, links, and
// text). A non-image document also counts.
func hasUnsupportedMedia(msg *telego.Message) bool {
	if msg == nil {
		return false
	}
	if msg.Video != nil || msg.Voice != nil || msg.VideoNote != nil ||
		msg.Audio != nil || msg.Sticker != nil || msg.Animation != nil {
		return true
	}
	if doc := msg.Document; doc != nil && !strings.HasPrefix(strings.ToLower(doc.MimeType), "image/") {
		return true
	}
	return false
}
