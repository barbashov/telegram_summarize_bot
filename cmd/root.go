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
		return runBot(cfg)
	},
}

func init() {
	cobra.OnInitialize(func() { logger.Init(debugFlag) })
	rootCmd.PersistentFlags().BoolVar(&debugFlag, "debug", false, "enable debug logging")
}

func Execute() {
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func runBot(cfg *config.Config) error {
	m := metrics.New()

	database, err := db.New(cfg.DBPath, m)
	if err != nil {
		return fmt.Errorf("failed to initialize database: %w", err)
	}
	m.InitLatencyStats(database)
	defer func() { _ = database.Close() }()

	{
		seedCtx, seedCancel := context.WithTimeout(context.Background(), 10*time.Second)
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

	tgBot, err := handlers.NewBot(cfg, database, sum, m)
	if err != nil {
		return fmt.Errorf("failed to initialize bot: %w", err)
	}

	startupCtx, startupCancel := context.WithTimeout(context.Background(), 10*time.Second)
	tgBot.NotifyUsers(startupCtx, "Бот запущен и в сети ✅")
	startupCancel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sigChan
		logger.Info().Msg("Received shutdown signal")

		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
		tgBot.NotifyUsers(shutdownCtx, "Бот остановлен ⛔")
		shutdownCancel()

		cancel()
	}()

	logger.Info().Msg("Starting bot...")
	if err := tgBot.Start(ctx); err != nil {
		logger.Error().Err(err).Msg("Bot stopped with error")
	}

	logger.Info().Msg("Bot shutdown complete")
	return nil
}
