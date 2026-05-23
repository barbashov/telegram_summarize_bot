package cmd

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"telegram_summarize_bot/config"
	"telegram_summarize_bot/db"
	"telegram_summarize_bot/handlers"
	"telegram_summarize_bot/logger"
	"telegram_summarize_bot/metrics"
	"telegram_summarize_bot/provider"
	"telegram_summarize_bot/summarizer"
)

var (
	cfg       *config.Config
	debugFlag bool
)

var rootCmd = &cobra.Command{
	Use:          "bot",
	Short:        "Telegram summarize bot",
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		var err error
		cfg, err = config.Load()
		if err != nil {
			return fmt.Errorf("failed to load config: %w", err)
		}
		return runBot(cmd.Context(), cfg)
	},
}

func init() {
	cobra.OnInitialize(func() { logger.Init(debugFlag) })
	rootCmd.PersistentFlags().BoolVar(&debugFlag, "debug", false, "enable debug logging")
}

func Execute() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	err := rootCmd.ExecuteContext(ctx)
	stop()
	if err != nil {
		os.Exit(1)
	}
}

// resolveVision returns (enabled, modelName). VISION_ENABLED overrides the
// auto-detection; "auto" delegates to the provider's capability check.
func resolveVision(cfg *config.Config, llmClient provider.LLMClient) (enabled bool, model string) {
	model = cfg.VisionModelOrDefault()
	switch cfg.VisionEnabled {
	case config.VisionEnabledFalse:
		return false, model
	case config.VisionEnabledTrue:
		return true, model
	}
	vc, ok := llmClient.(provider.VisionCapable)
	if !ok {
		return false, model
	}
	return vc.SupportsVision(model), model
}

func runBot(ctx context.Context, cfg *config.Config) error {
	m := metrics.New()

	database, err := db.New(cfg.DBPath, m)
	if err != nil {
		return fmt.Errorf("failed to initialize database: %w", err)
	}
	m.InitLatencyStats(database)
	defer func() { _ = database.Close() }()

	{
		seedCtx, seedCancel := context.WithTimeout(ctx, 10*time.Second)
		seedErr := database.SeedAllowedGroupsIfEmpty(seedCtx, cfg.AllowedGroups)
		seedCancel()
		if seedErr != nil {
			return fmt.Errorf("failed to seed allowed groups: %w", seedErr)
		}
	}

	logger.Info().
		Str("db_path", cfg.DBPath).
		Str("llm_mode", string(cfg.LLMMode)).
		Int("summary_hours", cfg.SummaryHours).
		Int("retention_days", cfg.RetentionDays).
		Int("max_messages", cfg.MaxMessages).
		Int("topic_max", cfg.TopicMax).
		Int("rate_limit_sec", cfg.RateLimitSec).
		Str("model", cfg.Model).
		Msg("Configuration loaded")

	llmClient, err := provider.New(cfg)
	if err != nil {
		return fmt.Errorf("failed to initialize LLM provider: %w", err)
	}

	sum := summarizer.New(llmClient, cfg.Model, m, cfg.ReplyThreads)

	initCtx, initCancel := context.WithTimeout(ctx, 10*time.Second)
	tgBot, err := handlers.NewBot(initCtx, cfg, database, sum, m)
	initCancel()
	if err != nil {
		return fmt.Errorf("failed to initialize bot: %w", err)
	}

	// Image-description support: enabled when the chosen vision model is
	// known to support multimodal input, gated by VISION_ENABLED. The Bot
	// implements summarizer.PhotoFetcher via its FetchImage method.
	if visionOn, visionModel := resolveVision(cfg, llmClient); visionOn {
		describer := summarizer.NewCachedDescriber(database, llmClient, tgBot, visionModel, cfg.ImageDescribeTimeout())
		sum.WithImageDescriber(database, describer, cfg.ImageDescribeConcurrency)
		logger.Info().Str("vision_model", visionModel).Msg("Image recognition enabled")
	} else {
		logger.Info().Msg("Image recognition disabled (model is not vision-capable or feature is off)")
	}

	startupCtx, startupCancel := context.WithTimeout(ctx, 10*time.Second)
	tgBot.NotifyUsers(startupCtx, "Бот запущен и в сети ✅")
	startupCancel()

	logger.Info().Msg("Starting bot...")
	startErr := tgBot.Start(ctx)

	// If shutdown was triggered by signal (vs. an internal error), notify
	// admins. Use a fresh background context with timeout since ctx is now
	// cancelled.
	if ctx.Err() != nil {
		logger.Info().Msg("Received shutdown signal")
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
		tgBot.NotifyUsers(shutdownCtx, "Бот остановлен ⛔")
		shutdownCancel()
	}

	if startErr != nil {
		logger.Error().Err(startErr).Msg("Bot stopped with error")
	}

	logger.Info().Msg("Bot shutdown complete")
	return nil
}
