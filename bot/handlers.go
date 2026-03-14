package bot

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"telegram_summarize_bot/config"
	"telegram_summarize_bot/db"
	"telegram_summarize_bot/logger"
	"telegram_summarize_bot/summarizer"

	"github.com/mymmrac/telego"
	tu "github.com/mymmrac/telego/telegoutil"
)

const telegramMessageLimit = 4096

type telegramClient interface {
	GetMe() (*telego.User, error)
	UpdatesViaLongPolling(params *telego.GetUpdatesParams, options ...telego.LongPollingOption) (<-chan telego.Update, error)
	StopLongPolling()
	SendMessage(params *telego.SendMessageParams) (*telego.Message, error)
	EditMessageText(params *telego.EditMessageTextParams) (*telego.Message, error)
}

type summaryService interface {
	SummarizeByTopics(ctx context.Context, messages []db.Message, topicMax int) (*summarizer.StructuredSummary, error)
}

func formatDuration(d time.Duration) string {
	seconds := int(d.Seconds())
	if seconds < 60 {
		return fmt.Sprintf("%d секунд", seconds)
	}
	minutes := seconds / 60
	return fmt.Sprintf("%d минут", minutes)
}

type Bot struct {
	telegram    telegramClient
	db          *db.DB
	summarizer  summaryService
	rateLimiter *RateLimiter
	cfg         *config.Config
	username    string
}

func NewBot(cfg *config.Config, database *db.DB, sum *summarizer.Summarizer) (*Bot, error) {
	bot, err := telego.NewBot(cfg.BotToken)
	if err != nil {
		return nil, fmt.Errorf("failed to create bot: %w", err)
	}

	me, err := bot.GetMe()
	if err != nil {
		return nil, fmt.Errorf("failed to get bot info: %w", err)
	}
	if me.Username == "" {
		return nil, fmt.Errorf("bot has no username")
	}

	return &Bot{
		telegram:    bot,
		db:          database,
		summarizer:  sum,
		rateLimiter: NewRateLimiter(cfg.RateLimitSec),
		cfg:         cfg,
		username:    strings.ToLower(me.Username),
	}, nil
}

func (b *Bot) Start(ctx context.Context) error {
	logger.Info().Msg("Starting Telegram bot with polling...")

	u := &telego.GetUpdatesParams{
		Offset:  0,
		Timeout: 60,
	}

	updates, err := b.telegram.UpdatesViaLongPolling(u)
	if err != nil {
		return fmt.Errorf("failed to start polling: %w", err)
	}

	go b.cleanupLoop(ctx)
	go b.rateLimitCleanupLoop(ctx)

	logger.Info().Msg("Bot started successfully, listening for updates...")

	for {
		select {
		case <-ctx.Done():
			logger.Info().Msg("Stopping bot...")
			b.telegram.StopLongPolling()
			return nil
		case update := <-updates:
			go b.handleUpdate(ctx, update)
		}
	}
}

func (b *Bot) cleanupLoop(ctx context.Context) {
	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			deleted, err := b.db.CleanupOldMessages(ctx, b.cfg.RetentionDuration())
			if err != nil {
				logger.Error().Err(err).Msg("failed to cleanup old messages")
			} else if deleted > 0 {
				logger.Info().Int64("deleted", deleted).Msg("cleaned up old messages")
			}
		}
	}
}

func (b *Bot) rateLimitCleanupLoop(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			b.rateLimiter.ClearOldEntries()
		}
	}
}

// originUsername returns a display name for the original author of a forwarded message.
func originUsername(origin telego.MessageOrigin) string {
	switch o := origin.(type) {
	case *telego.MessageOriginUser:
		if o.SenderUser.Username != "" {
			return o.SenderUser.Username
		}
		name := strings.TrimSpace(o.SenderUser.FirstName + " " + o.SenderUser.LastName)
		if name != "" {
			return name
		}
		return fmt.Sprintf("User%d", o.SenderUser.ID)
	case *telego.MessageOriginHiddenUser:
		return o.SenderUserName
	case *telego.MessageOriginChat:
		if o.AuthorSignature != "" {
			return o.AuthorSignature
		}
		return o.SenderChat.Title
	case *telego.MessageOriginChannel:
		if o.AuthorSignature != "" {
			return o.AuthorSignature
		}
		return o.Chat.Title
	default:
		return "unknown"
	}
}

func (b *Bot) handleUpdate(ctx context.Context, update telego.Update) {
	if update.Message == nil {
		return
	}

	msg := update.Message
	groupID := msg.Chat.ID
	userID := msg.From.ID
	text := msg.Text

	logger.Debug().
		Int64("group_id", groupID).
		Int64("user_id", userID).
		Str("text", text).
		Msg("Received message")

	if msg.From == nil {
		return
	}

	if text == "" {
		return
	}

	if msg.Chat.Type != "private" && !b.cfg.IsGroupAllowed(groupID) {
		logger.Warn().
			Int64("group_id", groupID).
			Int64("user_id", userID).
			Str("chat_type", msg.Chat.Type).
			Msg("ignoring message from non-allowed group")
		return
	}

	// Forwarded messages are stored with original author attribution but never
	// treated as commands — the forwarder didn't intend to issue one.
	if msg.ForwardOrigin != nil {
		forwardedFrom := originUsername(msg.ForwardOrigin)
		if err := b.db.AddMessage(ctx, &db.Message{
			GroupID:       groupID,
			UserID:        userID,
			Username:      msg.From.Username,
			Text:          text,
			Timestamp:     time.Now(),
			ForwardedFrom: forwardedFrom,
		}); err != nil {
			logger.Error().Err(err).Msg("failed to add forwarded message")
		}
		return
	}

	if msg.Chat.Type == "private" {
		b.handlePrivateChatInfo(ctx, update)
		return
	}

	command, err := b.extractCommandFromMention(text, msg.Entities)
	if err == nil {
		b.handleCommand(ctx, update, command)
		return
	}

	if err := b.db.AddMessage(ctx, &db.Message{
		GroupID:   groupID,
		UserID:    userID,
		Username:  msg.From.Username,
		Text:      text,
		Timestamp: time.Now(),
	}); err != nil {
		logger.Error().Err(err).Msg("failed to add message")
	}
}

func (b *Bot) handleCommand(ctx context.Context, update telego.Update, command string) {
	msg := update.Message
	if msg == nil {
		return
	}

	parts := strings.Fields(command)
	if len(parts) == 0 {
		b.handleHelp(ctx, update)
		return
	}

	cmd := parts[0]

	switch cmd {
	case "summarize", "sub", "s":
		b.handleSummarize(ctx, update, parts[1:])
	case "help":
		b.handleHelp(ctx, update)
	default:
		b.handleHelp(ctx, update)
	}
}

func (b *Bot) handleHelp(ctx context.Context, update telego.Update) {
	msg := update.Message
	if msg == nil {
		return
	}

	helpText := "📖 *Доступные команды:*\n\n" +
		"• `summarize [часы]` (или `s`, `sub`) — суммировать сообщения за последние N часов (по умолчанию 24)\n" +
		"• `help` — показать это сообщение\n\n" +
		"_Примеры: @bot summarize, @bot summarize 12_"

	b.sendMessage(ctx, msg.Chat.ID, helpText)
}

func (b *Bot) handlePrivateChatInfo(ctx context.Context, update telego.Update) {
	msg := update.Message
	if msg == nil {
		return
	}

	privateInfoText := "Я работаю только в группах и полезен для суммаризации групповых обсуждений.\n\n" +
		"Добавьте меня в группу и используйте `@" + b.username + " summarize`."

	b.sendMessage(ctx, msg.Chat.ID, privateInfoText)
}

func (b *Bot) extractCommandFromMention(text string, entities []telego.MessageEntity) (string, error) {
	mention := "@" + b.username

	if strings.HasPrefix(strings.ToLower(text), mention) {
		cmd := text[len(mention):]
		cmd = strings.TrimSpace(cmd)
		return cmd, nil
	}

	runes := []rune(text)
	for _, entity := range entities {
		entityType := string(entity.Type)
		if entityType == "mention" || entityType == "text_mention" {
			start := int(entity.Offset)
			end := start + int(entity.Length)
			if end > len(runes) {
				continue
			}
			entityText := string(runes[start:end])
			if strings.ToLower(entityText) == mention {
				cmd := strings.TrimSpace(string(runes[end:]))
				return cmd, nil
			}
		}
	}

	return "", fmt.Errorf("no bot mention found")
}

func (b *Bot) handleSummarize(ctx context.Context, update telego.Update, args []string) {
	msg := update.Message
	groupID := msg.Chat.ID
	userID := msg.From.ID

	hours := b.cfg.SummaryHours
	if len(args) > 0 {
		parsed, err := strconv.Atoi(args[0])
		if err != nil || parsed <= 0 {
			b.sendMessage(ctx, groupID, "Неверный формат. Используйте: @bot summarize [часы]\nПример: @bot summarize 12")
			return
		}
		if parsed > b.cfg.SummaryHours {
			b.sendMessage(ctx, groupID, fmt.Sprintf("Максимальный период суммаризации — %d часов.", b.cfg.SummaryHours))
			return
		}
		hours = parsed
	}

	lastSummarize, err := b.db.GetLastSummarizeTime(ctx, groupID)
	if err != nil {
		logger.Error().Err(err).Msg("failed to get last summarize time")
	}

	since := time.Now().Add(-time.Duration(hours) * time.Hour)
	if lastSummarize != nil && since.Before(*lastSummarize) {
		since = *lastSummarize
	}

	messages, err := b.db.GetMessages(ctx, groupID, since, b.cfg.MaxMessages)
	if err != nil {
		logger.Error().Err(err).Msg("failed to get messages")
		b.sendMessage(ctx, groupID, "Ошибка получения сообщений.")
		return
	}

	if len(messages) == 0 {
		if lastSummarize != nil {
			b.sendMessage(ctx, groupID, "Нет новых сообщений с последней суммаризации.")
		} else {
			b.sendMessage(ctx, groupID, "Нет сообщений за последние 24 часа.")
		}
		return
	}

	if !b.rateLimiter.Allow(userID, groupID) {
		remaining := b.rateLimiter.RemainingTime(groupID)
		b.sendMessage(ctx, groupID, "Подождите "+formatDuration(remaining)+" перед следующим запросом суммаризации.")
		return
	}

	committed := false
	defer func() {
		if !committed {
			b.rateLimiter.Release(groupID)
		}
	}()

	logger.Info().Int("count", len(messages)).Msg("Summarizing messages")

	statusMsgID := b.sendMessage(ctx, groupID, fmt.Sprintf("Собираю сообщения за последние %d часов...", hours))

	summary, err := b.summarizer.SummarizeByTopics(ctx, messages, b.cfg.TopicMax)
	if err != nil {
		logger.Error().Err(err).Msg("failed to summarize")
		if editErr := b.editMessage(groupID, statusMsgID, "Ошибка суммаризации. Попробуйте позже."); editErr != nil {
			b.sendMessage(ctx, groupID, "Ошибка суммаризации. Попробуйте позже.")
		}
		return
	}

	committed = true

	if err := b.db.SetLastSummarizeTime(ctx, groupID, time.Now()); err != nil {
		logger.Error().Err(err).Msg("failed to set last summarize time")
	}

	b.sendSummary(ctx, groupID, statusMsgID, summary)
}

func (b *Bot) sendMessage(ctx context.Context, chatID int64, text string) int64 {
	msg, err := b.telegram.SendMessage(tu.Message(
		tu.ID(chatID),
		text,
	).WithParseMode("Markdown"))
	if err != nil {
		logger.Error().Err(err).Int64("chat_id", chatID).Msg("failed to send message")
		return 0
	}
	return int64(msg.MessageID)
}

func (b *Bot) editMessage(chatID int64, messageID int64, text string) error {
	_, err := b.telegram.EditMessageText(&telego.EditMessageTextParams{
		ChatID:    tu.ID(chatID),
		MessageID: int(messageID),
		Text:      text,
		ParseMode: "Markdown",
	})
	if err != nil {
		logger.Error().Err(err).Int64("chat_id", chatID).Int64("message_id", messageID).Msg("failed to edit message")
	}
	return err
}

func (b *Bot) sendSummary(ctx context.Context, chatID, statusMsgID int64, summary *summarizer.StructuredSummary) {
	chunks := splitTelegramMessage(summarizer.FormatTelegramSummary(summary), telegramMessageLimit)
	if len(chunks) == 0 {
		chunks = []string{"📝 *Суммаризация:*\n\nНет данных для суммаризации."}
	}

	if err := b.editMessage(chatID, statusMsgID, chunks[0]); err != nil {
		b.sendMessage(ctx, chatID, chunks[0])
	}
	for _, chunk := range chunks[1:] {
		b.sendMessage(ctx, chatID, chunk)
	}
}

func splitTelegramMessage(text string, limit int) []string {
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
		candidate := line
		if current.Len() > 0 {
			candidate = current.String() + "\n" + line
		}

		if len(candidate) <= limit {
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

func (b *Bot) NotifyUsers(ctx context.Context, text string) (int, int) {
	attempted := len(b.cfg.AlertUserIDs)
	if attempted == 0 {
		return 0, 0
	}

	sent := 0
	failed := 0
	for _, userID := range b.cfg.AlertUserIDs {
		if b.sendMessage(ctx, userID, text) == 0 {
			failed++
			continue
		}
		sent++
	}

	logger.Info().
		Int("attempted", attempted).
		Int("sent", sent).
		Int("failed", failed).
		Msg("alert notifications sent")

	return sent, failed
}
