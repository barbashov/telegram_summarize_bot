package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"telegram_summarize_bot/bot"
	"telegram_summarize_bot/config"
	"telegram_summarize_bot/db"
	"telegram_summarize_bot/logger"
	"telegram_summarize_bot/summarizer"
)

func main() {
	logger.Init(true)

	cfg, err := config.Load()
	if err != nil {
		logger.Fatal().Err(err).Msg("Failed to load config")
	}

	database, err := db.New(cfg.DBPath)
	if err != nil {
		logger.Fatal().Err(err).Msg("Failed to initialize database")
	}
	defer database.Close()

	logger.Info().
		Str("db_path", cfg.DBPath).
		Int("summary_hours", cfg.SummaryHours).
		Int("retention_days", cfg.RetentionDays).
		Int("max_messages", cfg.MaxMessages).
		Int("rate_limit_sec", cfg.RateLimitSec).
		Str("model", cfg.Model).
		Msg("Configuration loaded")

	sum, err := summarizer.New(cfg.OpenRouterKey, cfg.OpenRouterURL, cfg.Model)
	if err != nil {
		logger.Fatal().Err(err).Msg("Failed to initialize summarizer")
	}

	tgBot, err := bot.NewBot(cfg, database, sum)
	if err != nil {
		logger.Fatal().Err(err).Msg("Failed to initialize bot")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sigChan
		logger.Info().Msg("Received shutdown signal")
		cancel()
	}()

	logger.Info().Msg("Starting bot...")
	if err := tgBot.Start(ctx); err != nil {
		logger.Error().Err(err).Msg("Bot stopped with error")
	}

	logger.Info().Msg("Bot shutdown complete")
}
