package bot

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"telegram_summarize_bot/config"
	"telegram_summarize_bot/db"
	"telegram_summarize_bot/fetcher"
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
	GetChat(params *telego.GetChatParams) (*telego.ChatFullInfo, error)
}

type summaryService interface {
	SummarizeByTopics(ctx context.Context, messages []db.Message, topicMax int) (*summarizer.StructuredSummary, error)
	SummarizeURL(ctx context.Context, pageURL string, content string) (string, error)
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
	telegram     telegramClient
	db           *db.DB
	summarizer   summaryService
	rateLimiter  *RateLimiter
	cfg          *config.Config
	username     string
	metrics      *metrics.Metrics
	userHashSalt []byte
}

func NewBot(cfg *config.Config, database *db.DB, sum *summarizer.Summarizer, m *metrics.Metrics) (*Bot, error) {
	bot, err := telego.NewBot(cfg.BotToken, telego.WithHTTPClient(buildHTTPClient(60*time.Second)))
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

	salt, err := b.db.GetUserHashSalt(ctx)
	if err != nil {
		return fmt.Errorf("failed to load user hash salt: %w", err)
	}
	b.userHashSalt = salt

	u := &telego.GetUpdatesParams{
		Offset:  0,
		Timeout: 60,
	}

	updates, err := b.telegram.UpdatesViaLongPolling(u)
	if err != nil {
		return fmt.Errorf("failed to start polling: %w", err)
	}

	b.scanKnownGroups(ctx)

	retentionCutoff := time.Now().Add(-b.cfg.RetentionDuration())
	if snap, err := b.db.LoadMetrics(ctx, retentionCutoff); err != nil {
		logger.Error().Err(err).Msg("failed to load persisted metrics; starting fresh")
	} else {
		b.metrics.LoadFromPersistable(snap)
		logger.Info().Msg("metrics loaded from DB")
	}
	go b.metricsFlushLoop(ctx)

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
		case update, ok := <-updates:
			if !ok {
				return nil
			}
			go b.handleUpdate(ctx, update)
		}
	}
}

func (b *Bot) scanKnownGroups(ctx context.Context) {
	ids, err := b.db.GetAllowedGroupIDs(ctx)
	if err != nil {
		logger.Error().Err(err).Msg("scanKnownGroups: failed to get allowed group IDs")
		return
	}
	for _, id := range ids {
		title := ""
		info, err := b.telegram.GetChat(&telego.GetChatParams{ChatID: tu.ID(id)})
		if err != nil {
			logger.Warn().Err(err).Int64("group_id", id).Msg("scanKnownGroups: failed to get chat info")
		} else {
			title = info.Title
		}
		username := ""
		if info != nil {
			username = info.Username
		}
		if err := b.db.UpsertKnownGroup(ctx, id, title, username); err != nil {
			logger.Error().Err(err).Int64("group_id", id).Msg("scanKnownGroups: failed to upsert known group")
		} else {
			logger.Info().Int64("group_id", id).Str("title", title).Str("username", username).Msg("scanKnownGroups: upserted known group")
		}
	}
}

func (b *Bot) metricsFlushLoop(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			b.flushMetrics()
		}
	}
}

func (b *Bot) flushMetrics() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := b.db.SaveMetrics(ctx, b.metrics.PersistableSnapshot()); err != nil {
		logger.Error().Err(err).Msg("failed to flush metrics to DB")
	}
}

// FlushMetrics is called once during graceful shutdown.
func (b *Bot) FlushMetrics() { b.flushMetrics() }

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
	text := msg.Text
	tgMessageID := int64(msg.MessageID)

	var replyToTgID int64
	if msg.ReplyToMessage != nil {
		replyToTgID = int64(msg.ReplyToMessage.MessageID)
	}

	logger.Debug().
		Int64("group_id", groupID).
		Str("text", text).
		Msg("Received message")

	if text == "" {
		return
	}

	if msg.Chat.Type != "private" {
		// Track group title even for non-allowed groups.
		if err := b.db.UpsertKnownGroup(ctx, groupID, msg.Chat.Title, msg.Chat.Username); err != nil {
			logger.Error().Err(err).Int64("group_id", groupID).Msg("failed to upsert known group")
		} else {
			logger.Debug().Int64("group_id", groupID).Str("title", msg.Chat.Title).Str("username", msg.Chat.Username).Msg("upserted known group")
		}
		allowed, err := b.db.IsGroupAllowed(ctx, groupID)
		if err != nil {
			logger.Error().Err(err).Int64("group_id", groupID).Msg("failed to check group allowlist")
			return
		}
		if !allowed {
			logger.Warn().
				Int64("group_id", groupID).
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
			UserHash:      db.UserHash(msg.From.ID, groupID, b.userHashSalt),
			Text:          text,
			Timestamp:     time.Now(),
			ForwardedFrom: forwardedFrom,
			TgMessageID:   tgMessageID,
			ReplyToTgID:   replyToTgID,
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
		GroupID:     groupID,
		UserHash:    db.UserHash(msg.From.ID, groupID, b.userHashSalt),
		Text:        text,
		Timestamp:   time.Now(),
		TgMessageID: tgMessageID,
		ReplyToTgID: replyToTgID,
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

	if err := b.db.UpsertKnownGroup(ctx, groupID, title, cmu.Chat.Username); err != nil {
		logger.Error().Err(err).Int64("group_id", groupID).Msg("failed to upsert known group on bot join")
	} else {
		logger.Info().Int64("group_id", groupID).Str("title", title).Str("username", cmu.Chat.Username).Msg("upserted known group on bot join")
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
		"• `summarize [часы]` \\(или `s`, `sub`\\) — суммировать сообщения за последние N часов \\(по умолчанию 24\\)\n" +
		"• `schedule` — показать расписание ежедневной сводки\n" +
		"• `help` — показать это сообщение\n\n" +
		"_Примеры: @bot summarize, @bot summarize 12_"

	if b.isGroupAdmin(msg.Chat.ID, msg.From.ID) {
		helpText += "\n\n*Команды администратора:*\n" +
			"• `schedule on` — включить ежедневную сводку\n" +
			"• `schedule off` — выключить ежедневную сводку\n" +
			"• `schedule ЧЧ:ММ` — установить время ежедневной сводки в UTC\n" +
			"• `schedule now` — запустить внеплановую сводку прямо сейчас\n\n" +
			"_Пример: @bot schedule 08:00_"
	}

	b.sendFormatted(msg.Chat.ID, helpText)
}

func (b *Bot) handlePrivateCommand(ctx context.Context, update telego.Update) {
	msg := update.Message
	if msg == nil {
		return
	}

	isAdmin := b.cfg.IsAdminUser(msg.From.ID)

	fields := strings.Fields(msg.Text)
	if len(fields) == 0 {
		if isAdmin {
			b.handlePrivateAdminHelp(msg.Chat.ID)
		} else {
			b.handlePrivateChatInfo(update)
		}
		return
	}

	cmd := strings.ToLower(fields[0])
	// Strip optional @botname suffix (e.g. /status@mybot).
	if atIdx := strings.Index(cmd, "@"); atIdx != -1 {
		cmd = cmd[:atIdx]
	}

	switch cmd {
	case "/status":
		if !isAdmin {
			b.sendMessage(msg.Chat.ID, "Нет доступа.")
			return
		}
		b.sendMessage(msg.Chat.ID, b.metrics.FormatStatusReport())
	case "/groups":
		if !isAdmin {
			b.sendMessage(msg.Chat.ID, "Нет доступа.")
			return
		}
		b.handlePrivateGroups(ctx, update, fields[1:])
	case "/help":
		if isAdmin {
			b.handlePrivateAdminHelp(msg.Chat.ID)
		} else {
			b.handlePrivateChatInfo(update)
		}
	default:
		if isAdmin {
			// Check if the message contains a URL to summarize.
			if u := extractURL(msg.Text, msg.Entities); u != "" {
				b.handleURLSummarize(ctx, msg.Chat.ID, u)
				return
			}
			b.handlePrivateAdminHelp(msg.Chat.ID)
		} else {
			b.handlePrivateChatInfo(update)
		}
	}
}

func extractURL(text string, entities []telego.MessageEntity) string {
	for _, e := range entities {
		if e.Type == "url" {
			runes := []rune(text)
			end := e.Offset + e.Length
			if end > len(runes) {
				continue
			}
			return string(runes[e.Offset:end])
		}
		if e.Type == "text_link" && e.URL != "" {
			return e.URL
		}
	}
	return ""
}

func (b *Bot) handleURLSummarize(ctx context.Context, chatID int64, rawURL string) {
	if !b.rateLimiter.Allow(chatID) {
		b.metrics.IncRateLimitHit()
		remaining := b.rateLimiter.RemainingTime(chatID)
		b.sendMessage(chatID, "Подождите "+formatDuration(remaining)+" перед следующим запросом.")
		return
	}

	statusMsgID := b.sendMessage(chatID, "Загружаю страницу...")

	content, err := fetcher.Fetch(ctx, rawURL, b.cfg.URLMaxChars)
	if err != nil {
		logger.Error().Err(err).Str("url", rawURL).Msg("failed to fetch URL")
		b.editOrSend(chatID, statusMsgID, "Не удалось загрузить страницу: "+err.Error())
		return
	}

	if editErr := b.editMessage(chatID, statusMsgID, "Суммаризую содержимое..."); editErr != nil {
		logger.Warn().Err(editErr).Msg("failed to update status message")
	}

	summary, err := b.summarizer.SummarizeURL(ctx, rawURL, content)
	if err != nil {
		b.metrics.IncSummarizeFail()
		logger.Error().Err(err).Str("url", rawURL).Msg("failed to summarize URL")
		b.editOrSend(chatID, statusMsgID, "Ошибка суммаризации. Попробуйте позже.")
		return
	}

	b.metrics.IncSummarizeOK()
	result := fmt.Sprintf("🔗 *Суммаризация URL:*\n\n%s", summarizer.EscapeMarkdown(summary))
	chunks := splitTelegramMessage(result, telegramMessageLimit)
	if len(chunks) == 0 {
		return
	}
	b.editOrSendFormatted(chatID, statusMsgID, chunks[0])
	for _, chunk := range chunks[1:] {
		b.sendFormatted(chatID, chunk)
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
	case "add":
		if len(args) < 2 {
			b.sendFormatted(chatID, "Использование: `/groups add <group_id>`")
			return
		}
		groupID, err := strconv.ParseInt(args[1], 10, 64)
		if err != nil {
			b.sendMessage(chatID, "Неверный ID группы.")
			return
		}
		// Best-effort title lookup; don't block add if group is unknown.
		title := fmt.Sprintf("%d", groupID)
		groups, err := b.db.GetKnownGroups(ctx)
		if err != nil {
			logger.Error().Err(err).Msg("failed to get known groups")
		} else {
			for i := range groups {
				if groups[i].GroupID == groupID {
					title = groups[i].Title
					break
				}
			}
		}
		if err := b.db.AddAllowedGroup(ctx, groupID, msg.From.ID); err != nil {
			logger.Error().Err(err).Msg("failed to add allowed group")
			b.sendMessage(chatID, "Ошибка добавления группы.")
			return
		}
		b.sendMessage(chatID, fmt.Sprintf("✅ %s добавлена.", title))
	case "remove":
		if len(args) < 2 {
			b.sendFormatted(chatID, "Использование: `/groups remove <group_id>`")
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
		if err := b.db.RemoveAllowedGroup(ctx, groupID); err != nil {
			logger.Error().Err(err).Msg("failed to remove allowed group")
			b.sendMessage(chatID, "Ошибка удаления группы.")
			return
		}
		b.sendMessage(chatID, fmt.Sprintf("❌ %s удалена.", found.Title))
	default:
		b.sendFormatted(chatID, "Неизвестная подкоманда\\. Используйте: `/groups`, `/groups add <id>`, `/groups remove <id>`")
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
		escapedTitle := summarizer.EscapeMarkdown(g.Title)
		title := escapedTitle
		if g.Username != "" {
			title = fmt.Sprintf("[%s](https://t.me/%s)", escapedTitle, g.Username)
		} else if g.GroupID < 0 {
			// Supergroup/channel: strip the -100 prefix for the t.me/c/ link
			chatID := (-g.GroupID) - 1_000_000_000_000
			if chatID > 0 {
				title = fmt.Sprintf("[%s](https://t.me/c/%d)", escapedTitle, chatID)
			}
		}
		fmt.Fprintf(&sb, "%s %s \\(%s\\)\n", status, title, summarizer.EscapeMarkdown(fmt.Sprintf("%d", g.GroupID)))
	}
	sb.WriteString("\nДля управления:\n• `/groups add <group_id>`\n• `/groups remove <group_id>`")
	b.sendFormatted(chatID, sb.String())
}

func (b *Bot) handlePrivateAdminHelp(chatID int64) {
	helpText := "*Команды администратора*\n\n" +
		"`/help` — показать это сообщение\n" +
		"`/status` — статус бота и метрики\n" +
		"`/groups` — список разрешённых групп\n" +
		"`/groups add <group_id>` — добавить группу\n" +
		"`/groups remove <group_id>` — удалить группу\n\n" +
		"*Суммаризация URL:*\nОтправьте ссылку — бот загрузит страницу и вернёт краткое содержание\\."
	b.sendFormatted(chatID, helpText)
}

func (b *Bot) handlePrivateChatInfo(update telego.Update) {
	msg := update.Message
	if msg == nil {
		return
	}

	privateInfoText := "Я работаю только в группах и полезен для суммаризации групповых обсуждений\\.\n\n" +
		"Добавьте меня в группу и используйте `@" + b.username + " summarize`\\."

	b.sendFormatted(msg.Chat.ID, privateInfoText)
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
	upperBound := time.Now()

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

	if !b.rateLimiter.Allow(groupID) {
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
		b.editOrSend(groupID, statusMsgID, "Ошибка суммаризации. Попробуйте позже.")
		return
	}

	committed = true
	b.metrics.IncSummarizeOK()

	if err := b.db.SetLastSummarizeTime(ctx, groupID, upperBound); err != nil {
		logger.Error().Err(err).Msg("failed to set last summarize time")
	}

	b.sendSummary(groupID, statusMsgID, summary)
}

func (b *Bot) sendMessage(chatID int64, text string) int64 {
	defer b.metrics.TelegramSend.Start()()
	msg, err := b.telegram.SendMessage(tu.Message(
		tu.ID(chatID),
		text,
	))
	if err != nil {
		logger.Error().Err(err).Int64("chat_id", chatID).Msg("failed to send message")
		b.metrics.RecordError("telegram_send", err.Error())
		return 0
	}
	return int64(msg.MessageID)
}

func (b *Bot) editOrSend(chatID, msgID int64, text string) {
	if editErr := b.editMessage(chatID, msgID, text); editErr != nil {
		b.sendMessage(chatID, text)
	}
}

func (b *Bot) editMessage(chatID, messageID int64, text string) error {
	defer b.metrics.TelegramEdit.Start()()
	_, err := b.telegram.EditMessageText(&telego.EditMessageTextParams{
		ChatID:    tu.ID(chatID),
		MessageID: int(messageID),
		Text:      text,
	})
	if err != nil {
		logger.Error().Err(err).Int64("chat_id", chatID).Int64("message_id", messageID).Msg("failed to edit message")
		b.metrics.RecordError("telegram_edit", err.Error())
	}
	return err
}

func (b *Bot) sendFormatted(chatID int64, text string) {
	defer b.metrics.TelegramSend.Start()()
	_, err := b.telegram.SendMessage(tu.Message(
		tu.ID(chatID),
		text,
	).WithParseMode("MarkdownV2"))
	if err != nil {
		logger.Error().Err(err).Int64("chat_id", chatID).Msg("failed to send formatted message")
		b.metrics.RecordError("telegram_send", err.Error())
	}
}

func (b *Bot) editFormatted(chatID, messageID int64, text string) error {
	defer b.metrics.TelegramEdit.Start()()
	_, err := b.telegram.EditMessageText(&telego.EditMessageTextParams{
		ChatID:    tu.ID(chatID),
		MessageID: int(messageID),
		Text:      text,
		ParseMode: "MarkdownV2",
	})
	if err != nil {
		logger.Error().Err(err).Int64("chat_id", chatID).Int64("message_id", messageID).Msg("failed to edit formatted message")
		b.metrics.RecordError("telegram_edit", err.Error())
	}
	return err
}

func (b *Bot) editOrSendFormatted(chatID, msgID int64, text string) {
	if editErr := b.editFormatted(chatID, msgID, text); editErr != nil {
		b.sendFormatted(chatID, text)
	}
}

func (b *Bot) sendSummary(chatID, statusMsgID int64, summary *summarizer.StructuredSummary) {
	chunks := splitTelegramMessage(summarizer.FormatTelegramSummary(summary, chatID), telegramMessageLimit)
	if len(chunks) == 0 {
		chunks = []string{"📝 *Суммаризация:*\n\nНет данных для суммаризации\\."}
	}

	b.editOrSendFormatted(chatID, statusMsgID, chunks[0])
	for _, chunk := range chunks[1:] {
		b.sendFormatted(chatID, chunk)
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
			b.sendFormatted(groupID, "⏰ Ежедневная сводка *отключена*\\.")
		} else {
			b.sendFormatted(groupID, fmt.Sprintf("⏰ Ежедневная сводка *включена*, время: *%02d:%02d UTC*\\.", s.Hour, s.Minute))
		}
		return
	}

	// Mutating operations require admin privileges.
	if !b.isGroupAdmin(groupID, msg.From.ID) {
		b.sendMessage(groupID, "Только администраторы группы могут изменять расписание.")
		return
	}

	arg := strings.ToLower(args[0])

	// "now" triggers an immediate unscheduled summary.
	if arg == "now" {
		if !b.isGroupAdmin(groupID, msg.From.ID) {
			b.sendMessage(groupID, "Только администраторы группы могут запускать внеплановую сводку.")
			return
		}
		b.sendFormatted(groupID, "🔄 Запускаю внеплановую сводку\\.\\.\\.")
		b.runScheduledSummary(ctx, groupID, time.Now())
		return
	}

	// Validate HH:MM format early (before DB fetch) so we can return fast on bad input.
	var parsedHour, parsedMinute int
	isTime := false
	if arg != "on" && arg != "off" {
		parts := strings.SplitN(arg, ":", 2)
		if len(parts) != 2 {
			b.sendFormatted(groupID, "Неверный формат\\. Используйте: `schedule on`, `schedule off`, `schedule now` или `schedule ЧЧ:ММ`\\.")
			return
		}
		h, err1 := strconv.Atoi(parts[0])
		m, err2 := strconv.Atoi(parts[1])
		if err1 != nil || err2 != nil || h < 0 || h > 23 || m < 0 || m > 59 {
			b.sendFormatted(groupID, "Неверное время\\. Используйте формат ЧЧ:ММ, например `07:00`\\.")
			return
		}
		parsedHour, parsedMinute, isTime = h, m, true
	}

	// Get or create the schedule record.
	s, err := b.db.GetGroupSchedule(ctx, groupID)
	if err != nil {
		logger.Error().Err(err).Msg("failed to get group schedule")
		b.sendMessage(groupID, "Ошибка получения расписания.")
		return
	}
	if s == nil {
		s = &db.GroupSchedule{GroupID: groupID, Hour: b.cfg.DailySummaryHour}
	}

	// Mutate schedule based on subcommand.
	switch {
	case arg == "off":
		s.Enabled = false
	case arg == "on":
		s.Enabled = true
	case isTime:
		s.Enabled = true
		s.Hour = parsedHour
		s.Minute = parsedMinute
	}

	if err := b.db.SetGroupSchedule(ctx, s); err != nil {
		logger.Error().Err(err).Msg("failed to set group schedule")
		b.sendMessage(groupID, "Ошибка сохранения расписания.")
		return
	}

	if s.Enabled {
		b.sendFormatted(groupID, fmt.Sprintf("⏰ Ежедневная сводка *включена*, время: *%02d:%02d UTC*\\.", s.Hour, s.Minute))
	} else {
		b.sendFormatted(groupID, "⏰ Ежедневная сводка *отключена*\\.")
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
				go b.runScheduledSummary(ctx, groupID, now)
			}
		}
	}
}

func (b *Bot) runScheduledSummary(ctx context.Context, groupID int64, now time.Time) {
	since := now.UTC().Add(-24 * time.Hour)
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

	preamble := "🌅 *Утренняя \\#сводка за последние 24 часа:*"
	chunks := splitTelegramMessage(summarizer.FormatTelegramSummary(summary, groupID), telegramMessageLimit)
	if len(chunks) == 0 {
		return
	}
	b.sendFormatted(groupID, preamble+"\n\n"+chunks[0])
	for _, chunk := range chunks[1:] {
		b.sendFormatted(groupID, chunk)
	}

	if err := b.db.UpdateLastDailySummary(ctx, groupID, now); err != nil {
		logger.Error().Err(err).Int64("group_id", groupID).Msg("failed to update last daily summary")
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
