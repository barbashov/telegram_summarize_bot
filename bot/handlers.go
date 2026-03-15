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
	"telegram_summarize_bot/metrics"
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
	GetChatMember(params *telego.GetChatMemberParams) (telego.ChatMember, error)
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
	metrics     *metrics.Metrics
}

func NewBot(cfg *config.Config, database *db.DB, sum *summarizer.Summarizer, m *metrics.Metrics) (*Bot, error) {
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
		metrics:     m,
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
	go b.schedulerLoop(ctx)

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
	// Handle bot membership changes (bot added to / removed from a group).
	if update.MyChatMember != nil {
		b.handleMyChatMember(ctx, update.MyChatMember)
		return
	}

	if update.Message == nil {
		return
	}

	msg := update.Message
	if msg.From == nil {
		return
	}

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

	if msg.Chat.Type != "private" {
		// Track group title even for non-allowed groups.
		if err := b.db.UpsertKnownGroup(ctx, groupID, msg.Chat.Title); err != nil {
			logger.Error().Err(err).Int64("group_id", groupID).Msg("failed to upsert known group")
		}
		allowed, err := b.db.IsGroupAllowed(ctx, groupID)
		if err != nil {
			logger.Error().Err(err).Int64("group_id", groupID).Msg("failed to check group allowlist")
			return
		}
		if !allowed {
			logger.Warn().
				Int64("group_id", groupID).
				Int64("user_id", userID).
				Str("chat_type", msg.Chat.Type).
				Msg("ignoring message from non-allowed group")
			return
		}
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
		} else {
			b.metrics.IncMessagesStored()
		}
		return
	}

	if msg.Chat.Type == "private" {
		b.handlePrivateCommand(ctx, update)
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
	} else {
		b.metrics.IncMessagesStored()
	}
}

func (b *Bot) handleMyChatMember(ctx context.Context, cmu *telego.ChatMemberUpdated) {
	newStatus := cmu.NewChatMember.MemberStatus()
	if newStatus != "member" && newStatus != "administrator" {
		return
	}

	groupID := cmu.Chat.ID
	title := cmu.Chat.Title

	if err := b.db.UpsertKnownGroup(ctx, groupID, title); err != nil {
		logger.Error().Err(err).Int64("group_id", groupID).Msg("failed to upsert known group on bot join")
	}

	msg := fmt.Sprintf("Бот добавлен в группу «%s» (%d).\nДля разрешения: /groups add %d", title, groupID, groupID)
	b.NotifyUsers(ctx, msg)
}

func (b *Bot) handleCommand(ctx context.Context, update telego.Update, command string) {
	msg := update.Message
	if msg == nil {
		return
	}

	parts := strings.Fields(command)
	if len(parts) == 0 {
		b.handleHelp(update)
		return
	}

	cmd := parts[0]

	switch cmd {
	case "summarize", "sub", "s":
		b.handleSummarize(ctx, update, parts[1:])
	case "schedule":
		b.handleSchedule(ctx, update, parts[1:])
	case "help":
		b.handleHelp(update)
	default:
		b.handleHelp(update)
	}
}

func (b *Bot) handleHelp(update telego.Update) {
	msg := update.Message
	if msg == nil {
		return
	}

	helpText := "📖 *Доступные команды:*\n\n" +
		"• `summarize [часы]` (или `s`, `sub`) — суммировать сообщения за последние N часов (по умолчанию 24)\n" +
		"• `schedule` — показать расписание ежедневной сводки\n" +
		"• `schedule on` — включить ежедневную сводку (только администраторы)\n" +
		"• `schedule off` — выключить ежедневную сводку (только администраторы)\n" +
		"• `schedule ЧЧ:ММ` — установить время ежедневной сводки в UTC (только администраторы)\n" +
		"• `help` — показать это сообщение\n\n" +
		"_Примеры: @bot summarize, @bot summarize 12, @bot schedule 08:00_"

	b.sendMessage(msg.Chat.ID, helpText)
}

func (b *Bot) handlePrivateCommand(ctx context.Context, update telego.Update) {
	msg := update.Message
	if msg == nil {
		return
	}

	fields := strings.Fields(msg.Text)
	if len(fields) == 0 {
		b.handlePrivateChatInfo(update)
		return
	}

	cmd := strings.ToLower(fields[0])
	// Strip optional @botname suffix (e.g. /status@mybot).
	if atIdx := strings.Index(cmd, "@"); atIdx != -1 {
		cmd = cmd[:atIdx]
	}

	switch cmd {
	case "/status":
		if !b.cfg.IsAdminUser(msg.From.ID) {
			b.sendMessage(msg.Chat.ID, "Нет доступа.")
			return
		}
		b.sendMessage(msg.Chat.ID, b.metrics.FormatStatusReport())
	case "/groups":
		if !b.cfg.IsAdminUser(msg.From.ID) {
			b.sendMessage(msg.Chat.ID, "Нет доступа.")
			return
		}
		b.handlePrivateGroups(ctx, update, fields[1:])
	default:
		b.handlePrivateChatInfo(update)
	}
}

func (b *Bot) handlePrivateGroups(ctx context.Context, update telego.Update, args []string) {
	msg := update.Message
	chatID := msg.Chat.ID

	if len(args) == 0 {
		b.sendPrivateGroupsList(ctx, chatID)
		return
	}

	subCmd := strings.ToLower(args[0])

	switch subCmd {
	case "add", "remove":
		if len(args) < 2 {
			b.sendMessage(chatID, "Использование: `/groups add <group_id>` или `/groups remove <group_id>`")
			return
		}
		groupID, err := strconv.ParseInt(args[1], 10, 64)
		if err != nil {
			b.sendMessage(chatID, "Неверный ID группы.")
			return
		}
		groups, err := b.db.GetKnownGroups(ctx)
		if err != nil {
			logger.Error().Err(err).Msg("failed to get known groups")
			b.sendMessage(chatID, "Ошибка получения списка групп.")
			return
		}
		var found *db.KnownGroup
		for i := range groups {
			if groups[i].GroupID == groupID {
				found = &groups[i]
				break
			}
		}
		if found == nil {
			b.sendMessage(chatID, fmt.Sprintf("Группа %d не найдена в списке известных групп.", groupID))
			b.sendPrivateGroupsList(ctx, chatID)
			return
		}
		if subCmd == "add" {
			if err := b.db.AddAllowedGroup(ctx, groupID, msg.From.ID); err != nil {
				logger.Error().Err(err).Msg("failed to add allowed group")
				b.sendMessage(chatID, "Ошибка добавления группы.")
				return
			}
			b.sendMessage(chatID, fmt.Sprintf("✅ %s добавлена.", found.Title))
		} else {
			if err := b.db.RemoveAllowedGroup(ctx, groupID); err != nil {
				logger.Error().Err(err).Msg("failed to remove allowed group")
				b.sendMessage(chatID, "Ошибка удаления группы.")
				return
			}
			b.sendMessage(chatID, fmt.Sprintf("❌ %s удалена.", found.Title))
		}
	default:
		b.sendMessage(chatID, "Неизвестная подкоманда. Используйте: `/groups`, `/groups add <id>`, `/groups remove <id>`")
	}
}

func (b *Bot) sendPrivateGroupsList(ctx context.Context, chatID int64) {
	groups, err := b.db.GetKnownGroups(ctx)
	if err != nil {
		logger.Error().Err(err).Msg("failed to get known groups")
		b.sendMessage(chatID, "Ошибка получения списка групп.")
		return
	}
	if len(groups) == 0 {
		b.sendMessage(chatID, "Нет известных групп.")
		return
	}

	var sb strings.Builder
	sb.WriteString("📋 *Известные группы:*\n\n")
	for _, g := range groups {
		status := "❌"
		if g.Allowed {
			status = "✅"
		}
		fmt.Fprintf(&sb, "%s %s (%d)\n", status, g.Title, g.GroupID)
	}
	sb.WriteString("\nДля управления:\n• `/groups add <group_id>`\n• `/groups remove <group_id>`")
	b.sendMessage(chatID, sb.String())
}

func (b *Bot) handlePrivateChatInfo(update telego.Update) {
	msg := update.Message
	if msg == nil {
		return
	}

	privateInfoText := "Я работаю только в группах и полезен для суммаризации групповых обсуждений.\n\n" +
		"Добавьте меня в группу и используйте `@" + b.username + " summarize`."

	b.sendMessage(msg.Chat.ID, privateInfoText)
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
		entityType := entity.Type
		if entityType == "mention" || entityType == "text_mention" {
			start := entity.Offset
			end := start + entity.Length
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
			b.sendMessage(groupID, "Неверный формат. Используйте: @bot summarize [часы]\nПример: @bot summarize 12")
			return
		}
		if parsed > b.cfg.SummaryHours {
			b.sendMessage(groupID, fmt.Sprintf("Максимальный период суммаризации — %d часов.", b.cfg.SummaryHours))
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
		b.sendMessage(groupID, "Ошибка получения сообщений.")
		return
	}

	if len(messages) == 0 {
		if lastSummarize != nil {
			b.sendMessage(groupID, "Нет новых сообщений с последней суммаризации.")
		} else {
			b.sendMessage(groupID, "Нет сообщений за последние 24 часа.")
		}
		return
	}

	if !b.rateLimiter.Allow(userID, groupID) {
		b.metrics.IncRateLimitHit()
		remaining := b.rateLimiter.RemainingTime(groupID)
		b.sendMessage(groupID, "Подождите "+formatDuration(remaining)+" перед следующим запросом суммаризации.")
		return
	}

	committed := false
	defer func() {
		if !committed {
			b.rateLimiter.Release(groupID)
		}
	}()

	logger.Info().Int("count", len(messages)).Msg("Summarizing messages")

	statusMsgID := b.sendMessage(groupID, fmt.Sprintf("Собираю сообщения за последние %d часов...", hours))

	summary, err := b.summarizer.SummarizeByTopics(ctx, messages, b.cfg.TopicMax)
	if err != nil {
		b.metrics.IncSummarizeFail()
		logger.Error().Err(err).Msg("failed to summarize")
		if editErr := b.editMessage(groupID, statusMsgID, "Ошибка суммаризации. Попробуйте позже."); editErr != nil {
			b.sendMessage(groupID, "Ошибка суммаризации. Попробуйте позже.")
		}
		return
	}

	committed = true
	b.metrics.IncSummarizeOK()

	if err := b.db.SetLastSummarizeTime(ctx, groupID, time.Now()); err != nil {
		logger.Error().Err(err).Msg("failed to set last summarize time")
	}

	b.sendSummary(groupID, statusMsgID, summary)
}

func (b *Bot) sendMessage(chatID int64, text string) int64 {
	defer b.metrics.TelegramSend.Start()()
	msg, err := b.telegram.SendMessage(tu.Message(
		tu.ID(chatID),
		text,
	).WithParseMode("Markdown"))
	if err != nil {
		logger.Error().Err(err).Int64("chat_id", chatID).Msg("failed to send message")
		b.metrics.RecordError("telegram_send", err.Error())
		return 0
	}
	return int64(msg.MessageID)
}

func (b *Bot) editMessage(chatID, messageID int64, text string) error {
	defer b.metrics.TelegramEdit.Start()()
	_, err := b.telegram.EditMessageText(&telego.EditMessageTextParams{
		ChatID:    tu.ID(chatID),
		MessageID: int(messageID),
		Text:      text,
		ParseMode: "Markdown",
	})
	if err != nil {
		logger.Error().Err(err).Int64("chat_id", chatID).Int64("message_id", messageID).Msg("failed to edit message")
		b.metrics.RecordError("telegram_edit", err.Error())
	}
	return err
}

func (b *Bot) sendSummary(chatID, statusMsgID int64, summary *summarizer.StructuredSummary) {
	chunks := splitTelegramMessage(summarizer.FormatTelegramSummary(summary), telegramMessageLimit)
	if len(chunks) == 0 {
		chunks = []string{"📝 *Суммаризация:*\n\nНет данных для суммаризации."}
	}

	if err := b.editMessage(chatID, statusMsgID, chunks[0]); err != nil {
		b.sendMessage(chatID, chunks[0])
	}
	for _, chunk := range chunks[1:] {
		b.sendMessage(chatID, chunk)
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

func (b *Bot) isGroupAdmin(groupID, userID int64) bool {
	member, err := b.telegram.GetChatMember(&telego.GetChatMemberParams{
		ChatID: tu.ID(groupID),
		UserID: userID,
	})
	if err != nil {
		logger.Error().Err(err).Msg("failed to get chat member")
		return false
	}
	status := member.MemberStatus()
	return status == "creator" || status == "administrator"
}

func (b *Bot) handleSchedule(ctx context.Context, update telego.Update, args []string) {
	msg := update.Message
	groupID := msg.Chat.ID

	if len(args) == 0 {
		// Show current schedule to anyone.
		s, err := b.db.GetGroupSchedule(ctx, groupID)
		if err != nil {
			logger.Error().Err(err).Msg("failed to get group schedule")
			b.sendMessage(groupID, "Ошибка получения расписания.")
			return
		}
		if s == nil || !s.Enabled {
			b.sendMessage(groupID, "⏰ Ежедневная сводка *отключена*.")
		} else {
			b.sendMessage(groupID, fmt.Sprintf("⏰ Ежедневная сводка *включена*, время: *%02d:%02d UTC*.", s.Hour, s.Minute))
		}
		return
	}

	// Mutating operations require admin privileges.
	if !b.isGroupAdmin(groupID, msg.From.ID) {
		b.sendMessage(groupID, "Только администраторы группы могут изменять расписание.")
		return
	}

	arg := strings.ToLower(args[0])

	switch arg {
	case "off":
		s, err := b.db.GetGroupSchedule(ctx, groupID)
		if err != nil {
			logger.Error().Err(err).Msg("failed to get group schedule")
			b.sendMessage(groupID, "Ошибка получения расписания.")
			return
		}
		if s == nil {
			s = &db.GroupSchedule{GroupID: groupID, Hour: b.cfg.DailySummaryHour}
		}
		s.Enabled = false
		if err := b.db.SetGroupSchedule(ctx, s); err != nil {
			logger.Error().Err(err).Msg("failed to set group schedule")
			b.sendMessage(groupID, "Ошибка сохранения расписания.")
			return
		}
		b.sendMessage(groupID, "⏰ Ежедневная сводка *отключена*.")

	case "on":
		s, err := b.db.GetGroupSchedule(ctx, groupID)
		if err != nil {
			logger.Error().Err(err).Msg("failed to get group schedule")
			b.sendMessage(groupID, "Ошибка получения расписания.")
			return
		}
		if s == nil {
			s = &db.GroupSchedule{GroupID: groupID, Hour: b.cfg.DailySummaryHour, Minute: 0}
		}
		s.Enabled = true
		if err := b.db.SetGroupSchedule(ctx, s); err != nil {
			logger.Error().Err(err).Msg("failed to set group schedule")
			b.sendMessage(groupID, "Ошибка сохранения расписания.")
			return
		}
		b.sendMessage(groupID, fmt.Sprintf("⏰ Ежедневная сводка *включена*, время: *%02d:%02d UTC*.", s.Hour, s.Minute))

	default:
		// Expect HH:MM format.
		parts := strings.SplitN(arg, ":", 2)
		if len(parts) != 2 {
			b.sendMessage(groupID, "Неверный формат. Используйте: `schedule on`, `schedule off` или `schedule ЧЧ:ММ`.")
			return
		}
		hour, err1 := strconv.Atoi(parts[0])
		minute, err2 := strconv.Atoi(parts[1])
		if err1 != nil || err2 != nil || hour < 0 || hour > 23 || minute < 0 || minute > 59 {
			b.sendMessage(groupID, "Неверное время. Используйте формат ЧЧ:ММ, например `07:00`.")
			return
		}
		s, err := b.db.GetGroupSchedule(ctx, groupID)
		if err != nil {
			logger.Error().Err(err).Msg("failed to get group schedule")
			b.sendMessage(groupID, "Ошибка получения расписания.")
			return
		}
		if s == nil {
			s = &db.GroupSchedule{GroupID: groupID}
		}
		s.Enabled = true
		s.Hour = hour
		s.Minute = minute
		if err := b.db.SetGroupSchedule(ctx, s); err != nil {
			logger.Error().Err(err).Msg("failed to set group schedule")
			b.sendMessage(groupID, "Ошибка сохранения расписания.")
			return
		}
		b.sendMessage(groupID, fmt.Sprintf("⏰ Ежедневная сводка *включена*, время: *%02d:%02d UTC*.", s.Hour, s.Minute))
	}
}

func (b *Bot) schedulerLoop(ctx context.Context) {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			now = now.UTC()
			schedules, err := b.db.GetEnabledSchedules(ctx)
			if err != nil {
				logger.Error().Err(err).Msg("failed to get enabled schedules")
				continue
			}
			for _, s := range schedules {
				if s.Hour != now.Hour() || s.Minute != now.Minute() {
					continue
				}
				today := now.Truncate(24 * time.Hour)
				if s.LastDailySummary != nil && !s.LastDailySummary.UTC().Truncate(24*time.Hour).Before(today) {
					continue
				}
				groupID := s.GroupID
				if err := b.db.UpdateLastDailySummary(ctx, groupID, now); err != nil {
					logger.Error().Err(err).Int64("group_id", groupID).Msg("failed to update last daily summary")
				}
				go b.runScheduledSummary(ctx, groupID)
			}
		}
	}
}

func (b *Bot) runScheduledSummary(ctx context.Context, groupID int64) {
	since := time.Now().UTC().Add(-24 * time.Hour)
	messages, err := b.db.GetMessages(ctx, groupID, since, b.cfg.MaxMessages)
	if err != nil {
		logger.Error().Err(err).Int64("group_id", groupID).Msg("scheduled summary: failed to get messages")
		return
	}
	if len(messages) == 0 {
		logger.Info().Int64("group_id", groupID).Msg("scheduled summary: no messages, skipping")
		return
	}

	logger.Info().Int64("group_id", groupID).Int("count", len(messages)).Msg("running scheduled summary")

	summary, err := b.summarizer.SummarizeByTopics(ctx, messages, b.cfg.TopicMax)
	if err != nil {
		b.metrics.IncSummarizeFail()
		logger.Error().Err(err).Int64("group_id", groupID).Msg("scheduled summary: failed to summarize")
		return
	}
	b.metrics.IncSummarizeOK()

	preamble := "🌅 *Утренняя сводка за последние 24 часа:*"
	chunks := splitTelegramMessage(summarizer.FormatTelegramSummary(summary), telegramMessageLimit)
	if len(chunks) == 0 {
		return
	}
	b.sendMessage(groupID, preamble+"\n\n"+chunks[0])
	for _, chunk := range chunks[1:] {
		b.sendMessage(groupID, chunk)
	}
}

func (b *Bot) NotifyUsers(ctx context.Context, text string) (sent, failed int) {
	attempted := len(b.cfg.AdminUserIDs)
	if attempted == 0 {
		return 0, 0
	}

	for _, userID := range b.cfg.AdminUserIDs {
		if b.sendMessage(userID, text) == 0 {
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
