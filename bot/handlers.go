package bot

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/mymmrac/telego"
	tu "github.com/mymmrac/telego/telegoutil"
	"telegram_summarize_bot/config"
	"telegram_summarize_bot/db"
	"telegram_summarize_bot/logger"
	"telegram_summarize_bot/summarizer"
)

func formatDuration(d time.Duration) string {
	seconds := int(d.Seconds())
	if seconds < 60 {
		return fmt.Sprintf("%d секунд", seconds)
	}
	minutes := seconds / 60
	return fmt.Sprintf("%d минут", minutes)
}

type Bot struct {
	telegram    *telego.Bot
	db          *db.DB
	summarizer  *summarizer.Summarizer
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

	if text == "" {
		return
	}

	if msg.Chat.Type == "private" {
		b.handleCommand(ctx, update, text)
		return
	}

	command, err := b.extractCommandFromMention(text, msg.Entities)
	if err == nil {
		b.handleCommand(ctx, update, command)
		return
	}

	b.db.AddMessage(ctx, &db.Message{
		GroupID:   groupID,
		UserID:    userID,
		Username:  msg.From.Username,
		Text:      text,
		Timestamp: time.Now(),
	})
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
		b.handleSummarize(ctx, update)
	case "add_admin":
		b.handleAddAdmin(ctx, update, parts)
	case "remove_admin":
		b.handleRemoveAdmin(ctx, update, parts)
	case "list_admins":
		b.handleListAdmins(ctx, update)
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
		"• `summarize` (или `s`, `sub`) — суммировать сообщения за последние 24 часа\n" +
		"• `help` — показать это сообщение\n\n" +
		"*Администрирование:*\n" +
		"• `add_admin <user_id>` — добавить админа в группу\n" +
		"• `remove_admin <user_id>` — удалить админа из группы\n" +
		"• `list_admins` — список админов группы\n\n" +
		"_Пример: @bot summarize_"

	b.sendMessage(ctx, msg.Chat.ID, helpText)
}

func (b *Bot) extractCommandFromMention(text string, entities []telego.MessageEntity) (string, error) {
	mention := "@" + b.username

	if strings.HasPrefix(text, mention) {
		cmd := strings.TrimPrefix(text, mention)
		cmd = strings.TrimSpace(cmd)
		return cmd, nil
	}

	for _, entity := range entities {
		entityType := string(entity.Type)
		if entityType == "mention" || entityType == "text_mention" {
			entityText := text[entity.Offset : entity.Offset+entity.Length]
			if strings.ToLower(entityText) == mention {
				cmd := text[entity.Offset+entity.Length:]
				cmd = strings.TrimSpace(cmd)
				return cmd, nil
			}
		}
	}

	return "", fmt.Errorf("no bot mention found")
}

func (b *Bot) handleSummarize(ctx context.Context, update telego.Update) {
	msg := update.Message
	groupID := msg.Chat.ID
	userID := msg.From.ID

	isAdmin, err := b.db.IsAdmin(ctx, groupID, userID)
	if err != nil {
		logger.Error().Err(err).Msg("failed to check admin")
		b.sendMessage(ctx, groupID, "Ошибка проверки прав доступа.")
		return
	}

	if !isAdmin {
		b.sendMessage(ctx, groupID, "У вас нет прав для выполнения этой команды.")
		return
	}

	if !b.rateLimiter.Allow(userID, groupID) {
		remaining := b.rateLimiter.RemainingTime(userID, groupID)
		b.sendMessage(ctx, groupID, "Подождите "+formatDuration(remaining)+" перед следующим запросом суммаризации.")
		return
	}

	lastSummarize, err := b.db.GetLastSummarizeTime(ctx, groupID)
	if err != nil {
		logger.Error().Err(err).Msg("failed to get last summarize time")
	}

	since := time.Now().Add(-b.cfg.SummaryDuration())
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

	logger.Info().Int("count", len(messages)).Msg("Summarizing messages")

	messagesText := b.db.FormatMessagesForSummary(messages)

	b.sendMessage(ctx, groupID, "Собираю сообщения за последние 24 часа...")

	summary, err := b.summarizer.Summarize(ctx, messagesText)
	if err != nil {
		logger.Error().Err(err).Msg("failed to summarize")
		b.sendMessage(ctx, groupID, "Ошибка суммаризации. Попробуйте позже.")
		return
	}

	b.db.SetLastSummarizeTime(ctx, groupID, time.Now())

	b.sendMessage(ctx, groupID, "📝 *Суммаризация:*\n\n"+summary)
}

func (b *Bot) handleAddAdmin(ctx context.Context, update telego.Update, parts []string) {
	msg := update.Message
	groupID := msg.Chat.ID
	userID := msg.From.ID

	isAdmin, err := b.db.IsAdmin(ctx, groupID, userID)
	if err != nil || !isAdmin {
		b.sendMessage(ctx, groupID, "У вас нет прав для выполнения этой команды.")
		return
	}

	if len(parts) < 2 {
		b.sendMessage(ctx, groupID, "Использование: add_admin <user_id>")
		return
	}

	newAdminID, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		b.sendMessage(ctx, groupID, "Неверный формат user_id.")
		return
	}

	if err := b.db.AddAdmin(ctx, groupID, newAdminID); err != nil {
		logger.Error().Err(err).Msg("failed to add admin")
		b.sendMessage(ctx, groupID, "Ошибка добавления админа.")
		return
	}

	b.sendMessage(ctx, groupID, fmt.Sprintf("Пользователь %d добавлен в список админов.", newAdminID))
}

func (b *Bot) handleRemoveAdmin(ctx context.Context, update telego.Update, parts []string) {
	msg := update.Message
	groupID := msg.Chat.ID
	userID := msg.From.ID

	isAdmin, err := b.db.IsAdmin(ctx, groupID, userID)
	if err != nil || !isAdmin {
		b.sendMessage(ctx, groupID, "У вас нет прав для выполнения этой команды.")
		return
	}

	if len(parts) < 2 {
		b.sendMessage(ctx, groupID, "Использование: remove_admin <user_id>")
		return
	}

	removeID, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		b.sendMessage(ctx, groupID, "Неверный формат user_id.")
		return
	}

	if err := b.db.RemoveAdmin(ctx, groupID, removeID); err != nil {
		logger.Error().Err(err).Msg("failed to remove admin")
		b.sendMessage(ctx, groupID, "Ошибка удаления админа.")
		return
	}

	b.sendMessage(ctx, groupID, fmt.Sprintf("Пользователь %d удален из списка админов.", removeID))
}

func (b *Bot) handleListAdmins(ctx context.Context, update telego.Update) {
	msg := update.Message
	groupID := msg.Chat.ID
	userID := msg.From.ID

	isAdmin, err := b.db.IsAdmin(ctx, groupID, userID)
	if err != nil || !isAdmin {
		b.sendMessage(ctx, groupID, "У вас нет прав для выполнения этой команды.")
		return
	}

	admins, err := b.db.GetAdmins(ctx, groupID)
	if err != nil {
		logger.Error().Err(err).Msg("failed to get admins")
		b.sendMessage(ctx, groupID, "Ошибка получения списка админов.")
		return
	}

	if len(admins) == 0 {
		b.sendMessage(ctx, groupID, "Список админов пуст.")
		return
	}

	var sb strings.Builder
	sb.WriteString("📋 *Список админов:*\n\n")
	for i, adminID := range admins {
		sb.WriteString(fmt.Sprintf("%d. %d\n", i+1, adminID))
	}

	b.sendMessage(ctx, groupID, sb.String())
}

func (b *Bot) sendMessage(ctx context.Context, chatID int64, text string) {
	_, err := b.telegram.SendMessage(tu.Message(
		tu.ID(chatID),
		text,
	).WithParseMode("Markdown"))
	if err != nil {
		logger.Error().Err(err).Int64("chat_id", chatID).Msg("failed to send message")
	}
}
