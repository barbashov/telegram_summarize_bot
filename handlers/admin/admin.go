package admin

import (
	"context"
	"strings"
	"time"

	"telegram_summarize_bot/config"
	"telegram_summarize_bot/db"
	"telegram_summarize_bot/logger"
	"telegram_summarize_bot/metrics"

	"github.com/mymmrac/telego"
)

// Deps provides messaging primitives that the admin package needs from the parent.
type Deps interface {
	SendMessage(chatID int64, text string) int64
	SendFormatted(chatID int64, text string)
	EditMessage(chatID, messageID int64, text string) error
	EditWithRetry(chatID, msgID int64, text string)
	EditFormattedWithRetry(chatID, msgID int64, text string)
}

// SummaryService abstracts the summarizer for URL summarization.
type SummaryService interface {
	SummarizeURL(ctx context.Context, pageURL string, content string) (string, error)
}

// RateLimiterIface abstracts the rate limiter.
type RateLimiterIface interface {
	Allow(groupID int64) bool
	RemainingTime(groupID int64) time.Duration
}

// TelegramSender is a subset of the Telegram client needed for direct API calls.
type TelegramSender interface {
	SendMessage(params *telego.SendMessageParams) (*telego.Message, error)
	AnswerCallbackQuery(params *telego.AnswerCallbackQueryParams) error
}

// Admin handles all admin-only private chat commands.
type Admin struct {
	deps        Deps
	db          *db.DB
	metrics     *metrics.Metrics
	cfg         *config.Config
	summarizer  SummaryService
	rateLimiter RateLimiterIface
	telegram    TelegramSender
}

// New creates a new Admin handler.
func New(deps Deps, database *db.DB, m *metrics.Metrics, cfg *config.Config, sum SummaryService, rl RateLimiterIface, tg TelegramSender) *Admin {
	return &Admin{
		deps:        deps,
		db:          database,
		metrics:     m,
		cfg:         cfg,
		summarizer:  sum,
		rateLimiter: rl,
		telegram:    tg,
	}
}

// Handle processes a private chat command from an admin user.
// Returns true if the command was handled, false otherwise.
func (a *Admin) Handle(ctx context.Context, update telego.Update) bool {
	msg := update.Message
	if msg == nil {
		return false
	}

	fields := strings.Fields(msg.Text)
	if len(fields) == 0 {
		a.handleHelp(msg.Chat.ID)
		return true
	}

	cmd := strings.ToLower(fields[0])
	if atIdx := strings.Index(cmd, "@"); atIdx != -1 {
		cmd = cmd[:atIdx]
	}

	switch cmd {
	case "/status":
		a.handleStatus(msg.Chat.ID)
	case "/reset":
		a.handleReset(ctx, msg.Chat.ID)
	case "/groups":
		a.handleGroups(ctx, msg.Chat.ID, msg.From.ID, fields[1:])
	case "/help":
		a.handleHelp(msg.Chat.ID)
	default:
		if u := extractURL(msg.Text, msg.Entities); u != "" {
			a.handleURLSummarize(ctx, msg.Chat.ID, u)
			return true
		}
		a.handleHelp(msg.Chat.ID)
	}
	return true
}

func (a *Admin) handleReset(ctx context.Context, chatID int64) {
	a.metrics.Reset()
	if err := a.db.ClearAllMetrics(ctx); err != nil {
		logger.Error().Err(err).Msg("failed to clear persisted metrics")
	}
	a.deps.SendMessage(chatID, "Метрики сброшены.")
}
