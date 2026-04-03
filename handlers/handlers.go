package handlers

import (
	"context"
	"fmt"
	"strings"
	"time"

	"telegram_summarize_bot/config"
	"telegram_summarize_bot/db"
	"telegram_summarize_bot/handlers/admin"
	"telegram_summarize_bot/logger"
	"telegram_summarize_bot/metrics"
	"telegram_summarize_bot/summarizer"

	"github.com/mymmrac/telego"
	tu "github.com/mymmrac/telego/telegoutil"
)

const (
	telegramMessageLimit     = 4096
	cleanupInterval          = 1 * time.Hour
	rateLimitCleanupInterval = 5 * time.Minute
)

type telegramClient interface {
	GetMe() (*telego.User, error)
	UpdatesViaLongPolling(params *telego.GetUpdatesParams, options ...telego.LongPollingOption) (<-chan telego.Update, error)
	StopLongPolling()
	SendMessage(params *telego.SendMessageParams) (*telego.Message, error)
	EditMessageText(params *telego.EditMessageTextParams) (*telego.Message, error)
	GetChatMember(params *telego.GetChatMemberParams) (telego.ChatMember, error)
	GetChat(params *telego.GetChatParams) (*telego.ChatFullInfo, error)
	SetMyCommands(params *telego.SetMyCommandsParams) error
	AnswerCallbackQuery(params *telego.AnswerCallbackQueryParams) error
}

type summaryService interface {
	SummarizeByTopics(ctx context.Context, messages []db.Message, topicMax int) (*summarizer.StructuredSummary, error)
	SummarizeURL(ctx context.Context, pageURL string, content string) (string, error)
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
	admin        *admin.Admin
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

	b := &Bot{
		telegram:    bot,
		db:          database,
		summarizer:  sum,
		rateLimiter: NewRateLimiter(cfg.RateLimitSec),
		cfg:         cfg,
		username:    strings.ToLower(me.Username),
		metrics:     m,
	}

	b.admin = admin.New(b, database, m, cfg, sum, b.rateLimiter, bot)

	return b, nil
}

func (b *Bot) Start(ctx context.Context) error {
	logger.Info().Msg("Starting Telegram bot with polling...")

	salt, err := b.db.GetUserHashSalt(ctx)
	if err != nil {
		return fmt.Errorf("failed to load user hash salt: %w", err)
	}
	b.userHashSalt = salt

	if err := b.telegram.SetMyCommands(&telego.SetMyCommandsParams{
		Commands: []telego.BotCommand{
			{Command: "status", Description: "Статус бота и метрики"},
			{Command: "reset", Description: "Сбросить все метрики"},
			{Command: "groups", Description: "Управление группами"},
			{Command: "help", Description: "Справка"},
		},
		Scope: tu.ScopeAllPrivateChats(),
	}); err != nil {
		logger.Warn().Err(err).Msg("failed to register bot commands")
	}

	u := &telego.GetUpdatesParams{
		Offset:  0,
		Timeout: 60,
		AllowedUpdates: []string{
			"message",
			"my_chat_member",
			"callback_query",
		},
	}

	updates, err := b.telegram.UpdatesViaLongPolling(u)
	if err != nil {
		return fmt.Errorf("failed to start polling: %w", err)
	}

	b.scanKnownGroups(ctx)

	b.refreshStatsCache()
	go b.statsCacheLoop(ctx)

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

// SendMessage sends a plain-text message and returns the message ID.
func (b *Bot) SendMessage(chatID int64, text string) int64 {
	return b.sendMessage(chatID, text)
}

// SendFormatted sends a MarkdownV2 message.
func (b *Bot) SendFormatted(chatID int64, text string) {
	b.sendFormatted(chatID, text)
}

// EditMessage edits a plain-text message.
func (b *Bot) EditMessage(chatID, messageID int64, text string) error {
	return b.editMessage(chatID, messageID, text)
}

// EditOrSend tries to edit a message; falls back to sending a new one.
func (b *Bot) EditOrSend(chatID, msgID int64, text string) {
	b.editOrSend(chatID, msgID, text)
}

// EditOrSendFormatted tries to edit a MarkdownV2 message; falls back to sending.
func (b *Bot) EditOrSendFormatted(chatID, msgID int64, text string) {
	b.editOrSendFormatted(chatID, msgID, text)
}

// TelegramClient returns the underlying Telegram client for direct API calls.
func (b *Bot) TelegramClient() telegramClient {
	return b.telegram
}
