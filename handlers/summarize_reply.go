package handlers

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"telegram_summarize_bot/db"
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

// handleSummarizeReply handles "@bot summarize" sent as a reply. When reply
// threading is on and the replied-to message belongs to a stored reply chain, it
// summarizes the whole branch; otherwise it acts on the single replied-to
// message (link(s), image(s), and/or text).
func (b *Bot) handleSummarizeReply(ctx context.Context, update telego.Update, steering string) {
	groupID := update.Message.Chat.ID
	reply := update.Message.ReplyToMessage

	if chain := b.replyChain(ctx, groupID, reply); len(chain) >= 2 {
		b.summarizeReplyThread(ctx, groupID, reply, chain, steering)
		return
	}
	b.summarizeSingleReply(ctx, groupID, reply, steering)
}

// replyChain reconstructs the reply branch ending at the replied-to message,
// ordered root→target, from the DB. Returns nil (→ single-message handling) when
// threading is off, the target isn't stored, or it has no resolvable ancestors.
func (b *Bot) replyChain(ctx context.Context, groupID int64, reply *telego.Message) []db.Message {
	if !b.cfg.ReplyThreads {
		return nil
	}
	target, err := b.db.GetMessageByTgID(ctx, groupID, int64(reply.MessageID))
	if err != nil || target == nil {
		return nil
	}
	lookup := func(tgID int64) (*db.Message, error) {
		return b.db.GetMessageByTgID(ctx, groupID, tgID)
	}
	chain, err := summarizer.WalkAncestry(*target, lookup, b.cfg.ReplyChainMaxDepth)
	if err != nil || len(chain) < 2 {
		return nil
	}
	return chain
}

// summarizeSingleReply acts on the single replied-to message (no resolvable
// ancestors): summarize its link(s), describe its image(s), and/or summarize its
// text, blended into one unified summary; a lone link or image short-circuits.
func (b *Bot) summarizeSingleReply(ctx context.Context, groupID int64, reply *telego.Message, steering string) {
	text, entities := replyTextAndEntities(reply)
	links := tgutil.ExtractURLs(text, entities, replyMaxLinks)
	photos := extractPhotoRecords(reply)
	// prose is the message text with the URL/anchor spans removed, so a bare
	// link ("https://…") leaves nothing and short-circuits to a plain link
	// summary, while genuine surrounding prose is still taken into account.
	prose := strings.TrimSpace(residualText(text, entities))

	hasOther := len(links) > 0 || len(photos) > 0
	// Plain text is only worth summarizing on its own above a threshold; when
	// there's also a link or image — or the user gave an explicit steering
	// prompt — the prose is folded in regardless of length.
	includeText := prose != "" && (steering != "" || utf8.RuneCountInString(prose) >= b.cfg.ReplyMinChars || hasOther)

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

	instructions := combineInstructions(b.loadGroupSummaryInstructions(ctx, groupID), steering)

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
		desc, derr := b.summarizer.DescribeImage(ctx, photo, steering)
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

	// The LLM result is Markdown; convert it (plus our header) to Telegram
	// MarkdownV2 so **bold**, lists, links etc. render instead of leaking as
	// literal markers.
	chunks := renderMarkdown("📝 **Суммаризация:**\n\n" + result)
	if len(chunks) == 0 {
		b.editWithRetry(ctx, groupID, statusMsgID, "Нет данных для суммаризации.")
		return
	}
	if err := b.editFormattedFinal(ctx, groupID, statusMsgID, chunks[0]); err != nil {
		logger.Error().Err(err).Int64("chat_id", groupID).Msg("reply-summarize: failed to send result")
		return
	}
	for _, chunk := range chunks[1:] {
		b.sendFormatted(ctx, groupID, chunk)
	}
	committed = true
}

// combineInstructions merges a group's saved summarization instructions with a
// per-message steering prompt, prioritizing the user's immediate ask. Either may
// be empty.
func combineInstructions(group, steering string) string {
	group = strings.TrimSpace(group)
	steering = strings.TrimSpace(steering)
	switch {
	case steering == "":
		return group
	case group == "":
		return "Запрос пользователя (приоритетно): " + steering
	default:
		return group + "\n\nЗапрос пользователя (приоритетно): " + steering
	}
}

// summarizeReplyThread summarizes a full reply branch (chain ordered root→target)
// as one conversation: every message gets full treatment (its text, followed
// links, described images) within chain-wide budgets, then the transcript is
// summarized — honoring any steering prompt over the whole thread.
func (b *Bot) summarizeReplyThread(ctx context.Context, groupID int64, reply *telego.Message, chain []db.Message, steering string) {
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

	statusMsgID := b.sendMessageReply(ctx, groupID, int64(reply.MessageID), "Собираю ветку обсуждения...")
	instructions := combineInstructions(b.loadGroupSummaryInstructions(ctx, groupID), steering)

	aliases := summarizer.BuildUserAliasMap(chain)
	linkBudget := b.cfg.ReplyChainMaxLinks
	imageBudget := b.cfg.ReplyChainMaxImages
	seenImg := map[string]string{} // file_unique_id → description, deduped chain-wide

	// Enrich target-first so the most relevant message gets budget priority; keep
	// the transcript in root→target order.
	blocks := make([]string, len(chain))
	for i := len(chain) - 1; i >= 0; i-- {
		m := chain[i]
		isTarget := i == len(chain)-1

		var links []string
		var photos []db.PhotoRecord
		body := strings.TrimSpace(m.Text)
		if isTarget {
			// Target: use the live message (entities + fresh photo handles).
			t, ents := replyTextAndEntities(reply)
			links = tgutil.ExtractURLs(t, ents, replyMaxLinks)
			photos = extractPhotoRecords(reply)
			if p := strings.TrimSpace(residualText(t, ents)); p != "" {
				body = p
			}
		} else {
			// Ancestor (stored, no entities): scan plain text + stored photos.
			links = tgutil.ExtractURLsFromText(m.Text, replyMaxLinks)
			if recs, perr := b.db.GetPhotosForMessages(ctx, []int64{m.ID}); perr == nil {
				photos = recs[m.ID]
			}
		}

		var sb strings.Builder
		fmt.Fprintf(&sb, "%s [%s]: %s", aliasOrAnon(aliases, m.UserHash), m.Timestamp.Format("15:04"), body)
		if m.ForwardedFrom != "" {
			fmt.Fprintf(&sb, " (переслано от %s)", m.ForwardedFrom)
		}

		for _, p := range photos {
			if p.FileUniqueID == "" {
				continue
			}
			desc, known := seenImg[p.FileUniqueID]
			if !known {
				if imageBudget <= 0 {
					continue
				}
				d, derr := b.summarizer.DescribeImage(ctx, p, steering)
				if errors.Is(derr, summarizer.ErrVisionDisabled) {
					break
				}
				desc = ""
				if derr == nil {
					desc = strings.TrimSpace(d)
				}
				seenImg[p.FileUniqueID] = desc // cache result (incl. empty) for this request
				if desc != "" {
					imageBudget--
				}
			}
			if desc != "" {
				fmt.Fprintf(&sb, "\n  [изображение: %s]", desc)
			}
		}

		for _, link := range links {
			if linkBudget <= 0 {
				break
			}
			content, ferr := b.fetchURL(ctx, link, b.cfg.URLMaxChars)
			if ferr != nil {
				continue
			}
			summary, serr := b.summarizer.SummarizeURL(ctx, link, content, instructions)
			if serr != nil || strings.TrimSpace(summary) == "" {
				continue
			}
			linkBudget--
			fmt.Fprintf(&sb, "\n  [ссылка %s: %s]", link, strings.TrimSpace(summary))
		}

		blocks[i] = sb.String()
	}

	var material strings.Builder
	material.WriteString("Ниже — ветка переписки Telegram, от начала к последнему сообщению, на которое ответили:\n\n")
	for _, blk := range blocks {
		material.WriteString(blk)
		material.WriteString("\n\n")
	}

	summary, err := b.summarizer.SummarizeText(ctx, strings.TrimSpace(material.String()), instructions)
	if err != nil {
		logger.Error().Err(err).Msg("reply-summarize: failed to summarize thread")
		b.editWithRetry(ctx, groupID, statusMsgID, "Ошибка суммаризации. Попробуйте позже.")
		return
	}
	result := strings.TrimSpace(summary)
	if result == "" {
		b.editWithRetry(ctx, groupID, statusMsgID, "Нет данных для суммаризации.")
		return
	}

	chunks := renderMarkdown("📝 **Суммаризация ветки:**\n\n" + result)
	if len(chunks) == 0 {
		b.editWithRetry(ctx, groupID, statusMsgID, "Нет данных для суммаризации.")
		return
	}
	if err := b.editFormattedFinal(ctx, groupID, statusMsgID, chunks[0]); err != nil {
		logger.Error().Err(err).Int64("chat_id", groupID).Msg("reply-summarize: failed to send thread result")
		return
	}
	for _, chunk := range chunks[1:] {
		b.sendFormatted(ctx, groupID, chunk)
	}
	committed = true
}

// aliasOrAnon returns the alias for a user hash, or "anon" when unknown/empty.
func aliasOrAnon(aliases map[string]string, hash string) string {
	if a := aliases[hash]; a != "" {
		return a
	}
	return "anon"
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
