package handlers

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"telegram_summarize_bot/config"
	"telegram_summarize_bot/db"
	"telegram_summarize_bot/fetcher"
	"telegram_summarize_bot/handlers/admin"
	"telegram_summarize_bot/httputil"
	"telegram_summarize_bot/logger"
	"telegram_summarize_bot/metrics"
	"telegram_summarize_bot/provider"
	"telegram_summarize_bot/summarizer"

	"github.com/mymmrac/telego"
	tu "github.com/mymmrac/telego/telegoutil"
)

const (
	telegramMessageLimit     = 4096
	cleanupInterval          = 1 * time.Hour
	rateLimitCleanupInterval = 5 * time.Minute
	// maxConcurrentUpdates bounds how many updates are processed in parallel,
	// providing backpressure against floods (each handler can do DB + LLM work).
	maxConcurrentUpdates = 32
	// shutdownDrainTimeout is how long Start waits for in-flight handlers to
	// finish before returning (and the caller closes the DB).
	shutdownDrainTimeout = 5 * time.Second
)

type telegramClient interface {
	GetMe(ctx context.Context) (*telego.User, error)
	UpdatesViaLongPolling(ctx context.Context, params *telego.GetUpdatesParams, options ...telego.LongPollingOption) (<-chan telego.Update, error)
	SendMessage(ctx context.Context, params *telego.SendMessageParams) (*telego.Message, error)
	EditMessageText(ctx context.Context, params *telego.EditMessageTextParams) (*telego.Message, error)
	GetChatMember(ctx context.Context, params *telego.GetChatMemberParams) (telego.ChatMember, error)
	GetChat(ctx context.Context, params *telego.GetChatParams) (*telego.ChatFullInfo, error)
	SetMyCommands(ctx context.Context, params *telego.SetMyCommandsParams) error
	AnswerCallbackQuery(ctx context.Context, params *telego.AnswerCallbackQueryParams) error
	GetFile(ctx context.Context, params *telego.GetFileParams) (*telego.File, error)
}

type summaryService interface {
	SummarizeByTopics(ctx context.Context, messages []db.Message, topicMax int, additionalInstructions string) (*summarizer.StructuredSummary, error)
	SummarizeURL(ctx context.Context, pageURL string, content string, instructions string) (string, error)
	SummarizeText(ctx context.Context, content string, instructions string) (string, error)
	DescribeImage(ctx context.Context, photo db.PhotoRecord, steering string) (string, error)
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
	// fetchURL fetches and extracts readable text from a URL. Defaults to
	// fetcher.Fetch; overridable in tests to avoid real network access.
	fetchURL func(ctx context.Context, rawURL string, maxChars int) (string, error)

	// inflight tracks running update handlers so shutdown can drain them; sem
	// bounds their concurrency (backpressure).
	inflight sync.WaitGroup
	sem      chan struct{}
}

func NewBot(ctx context.Context, cfg *config.Config, database *db.DB, sum *summarizer.Summarizer, m *metrics.Metrics, llm provider.LLMClient) (*Bot, error) {
	bot, err := telego.NewBot(cfg.BotToken, telego.WithHTTPClient(httputil.NewClient(60*time.Second)))
	if err != nil {
		return nil, fmt.Errorf("failed to create bot: %w", err)
	}

	me, err := bot.GetMe(ctx)
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
		fetchURL:    fetcher.Fetch,
		sem:         make(chan struct{}, maxConcurrentUpdates),
	}

	b.admin = admin.New(b, database, m, cfg, sum, b.rateLimiter, bot, llm)

	return b, nil
}

func (b *Bot) Start(ctx context.Context) error {
	logger.Info().Msg("Starting Telegram bot with polling...")

	salt, err := b.db.GetUserHashSalt(ctx)
	if err != nil {
		return fmt.Errorf("failed to load user hash salt: %w", err)
	}
	b.userHashSalt = salt

	if err := b.telegram.SetMyCommands(ctx, &telego.SetMyCommandsParams{
		Commands: []telego.BotCommand{
			{Command: "status", Description: "Статус бота и метрики"},
			{Command: "reset", Description: "Сбросить все метрики"},
			{Command: "groups", Description: "Управление группами"},
			{Command: "instructions", Description: "Инструкции суммаризации"},
			{Command: "usage", Description: "Использование токенов и квоты"},
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

	pollingCtx, cancelPolling := context.WithCancel(ctx)
	defer cancelPolling()

	updates, err := b.telegram.UpdatesViaLongPolling(pollingCtx, u)
	if err != nil {
		return fmt.Errorf("failed to start polling: %w", err)
	}

	b.scanKnownGroups(ctx)

	b.refreshStatsCache(ctx)
	go b.statsCacheLoop(ctx)

	go b.cleanupLoop(ctx)
	go b.rateLimitCleanupLoop(ctx)
	go b.schedulerLoop(ctx)

	logger.Info().Msg("Bot started successfully, listening for updates...")

	for {
		select {
		case <-ctx.Done():
			logger.Info().Msg("Stopping bot...")
			cancelPolling()
			b.drainHandlers(shutdownDrainTimeout)
			return nil
		case update, ok := <-updates:
			if !ok {
				return nil
			}
			// Acquire a slot before spawning so a flood applies backpressure to
			// the poller instead of spawning unbounded goroutines.
			select {
			case b.sem <- struct{}{}:
			case <-ctx.Done():
				logger.Info().Msg("Stopping bot...")
				cancelPolling()
				b.drainHandlers(shutdownDrainTimeout)
				return nil
			}
			b.inflight.Add(1)
			go func(u telego.Update) {
				defer b.inflight.Done()
				defer func() { <-b.sem }()
				b.handleUpdate(ctx, u)
			}(update)
		}
	}
}

// drainHandlers waits up to timeout for in-flight update handlers to finish, so
// they don't write to the DB after the caller closes it. In-flight LLM/network
// calls use the (now-cancelled) parent context and abort quickly; this mainly
// lets handlers unwind cleanly.
func (b *Bot) drainHandlers(timeout time.Duration) {
	done := make(chan struct{})
	go func() {
		b.inflight.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
		logger.Warn().Dur("timeout", timeout).Msg("shutdown drain timed out; some handlers still running")
	}
}

// SendMessage sends a plain-text message and returns the message ID.
func (b *Bot) SendMessage(ctx context.Context, chatID int64, text string) int64 {
	return b.sendMessage(ctx, chatID, text)
}

// SendFormatted sends a MarkdownV2 message.
func (b *Bot) SendFormatted(ctx context.Context, chatID int64, text string) {
	b.sendFormatted(ctx, chatID, text)
}

// EditMessage edits a plain-text message.
func (b *Bot) EditMessage(ctx context.Context, chatID, messageID int64, text string) error {
	return b.editMessage(ctx, chatID, messageID, text)
}

// EditWithRetry tries to edit a message, retrying on failure.
func (b *Bot) EditWithRetry(ctx context.Context, chatID, msgID int64, text string) {
	b.editWithRetry(ctx, chatID, msgID, text)
}

// EditFormattedWithRetry tries to edit a MarkdownV2 message, retrying on failure.
func (b *Bot) EditFormattedWithRetry(ctx context.Context, chatID, msgID int64, text string) {
	b.editFormattedWithRetry(ctx, chatID, msgID, text)
}

// TelegramClient returns the underlying Telegram client for direct API calls.
func (b *Bot) TelegramClient() telegramClient {
	return b.telegram
}
