package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"time"

	"telegram_summarize_bot/cmd"
	"telegram_summarize_bot/config"
	"telegram_summarize_bot/db"
	"telegram_summarize_bot/handlers"
	"telegram_summarize_bot/logger"
	"telegram_summarize_bot/metrics"
	"telegram_summarize_bot/provider"
	"telegram_summarize_bot/summarizer"
)

func main() {
	logger.Init(true)

	cfg, err := config.Load()
	if err != nil {
		logger.Fatal().Err(err).Msg("Failed to load config")
	}

	// Handle subcommands before full bot startup
	if len(os.Args) >= 3 && os.Args[1] == "openai" {
		switch os.Args[2] {
		case "auth":
			if len(os.Args) >= 4 && os.Args[3] == "token-refresh" {
				if err := cmd.RunTokenRefresh(cfg.OAuthClientID, cfg.OAuthTokenDir); err != nil {
					logger.Fatal().Err(err).Msg("Token refresh failed")
				}
			} else {
				if err := cmd.RunAuth(cfg.OAuthClientID, cfg.OAuthTokenDir); err != nil {
					logger.Fatal().Err(err).Msg("OAuth authentication failed")
				}
			}
			return
		case "models":
			if err := cmd.RunModels(cfg.OAuthClientID, cfg.OAuthTokenDir); err != nil {
				logger.Fatal().Err(err).Msg("Model listing failed")
			}
			return
		case "test":
			if len(os.Args) < 4 {
				logger.Fatal().Msg("Usage: bot openai test MODEL_NAME")
			}
			if err := cmd.RunTest(cfg.OAuthClientID, cfg.OAuthTokenDir, os.Args[3]); err != nil {
				logger.Fatal().Err(err).Msg("Model test failed")
			}
			return
		}
	}

	m := metrics.New()

	database, err := db.New(cfg.DBPath, m)
	if err != nil {
		logger.Fatal().Err(err).Msg("Failed to initialize database")
	}
	m.InitLatencyStats(database)
	defer func() { _ = database.Close() }()

	{
		seedCtx, seedCancel := context.WithTimeout(context.Background(), 10*time.Second)
		seedErr := database.SeedAllowedGroupsIfEmpty(seedCtx, cfg.AllowedGroups)
		seedCancel()
		if seedErr != nil {
			logger.Fatal().Err(seedErr).Msg("Failed to seed allowed groups")
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
		logger.Fatal().Err(err).Msg("Failed to initialize LLM provider")
	}

	sum := summarizer.New(llmClient, cfg.Model, m, cfg.ReplyThreads)

	tgBot, err := handlers.NewBot(cfg, database, sum, m)
	if err != nil {
		logger.Fatal().Err(err).Msg("Failed to initialize bot")
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
}
