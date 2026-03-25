package main

import (
	"context"
	"flag"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/sashabaranov/go-openai"
	"telegram_summarize_bot/bot"
	"telegram_summarize_bot/config"
	"telegram_summarize_bot/db"
	"telegram_summarize_bot/logger"
	"telegram_summarize_bot/metrics"
	"telegram_summarize_bot/rag"
	"telegram_summarize_bot/summarizer"
)

func main() {
	backfillPath := flag.String("backfill", "", "path to Telegram JSON export file for RAG backfill")
	backfillGroupID := flag.Int64("group-id", 0, "group ID for backfill")
	backfillReset := flag.Bool("reset", false, "wipe group vectors before backfill")
	flag.Parse()

	logger.Init(true)

	cfg, err := config.Load()
	if err != nil {
		logger.Fatal().Err(err).Msg("Failed to load config")
	}

	m := metrics.New()

	database, err := db.New(cfg.DBPath, m)
	if err != nil {
		logger.Fatal().Err(err).Msg("Failed to initialize database")
	}
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
		Int("summary_hours", cfg.SummaryHours).
		Int("retention_days", cfg.RetentionDays).
		Int("max_messages", cfg.MaxMessages).
		Int("topic_max", cfg.TopicMax).
		Int("rate_limit_sec", cfg.RateLimitSec).
		Str("model", cfg.Model).
		Bool("rag_enabled", cfg.RAGEnabled()).
		Msg("Configuration loaded")

	sum, err := summarizer.New(cfg.OpenRouterKey, cfg.OpenRouterURL, cfg.Model, m, cfg.ReplyThreads)
	if err != nil {
		logger.Fatal().Err(err).Msg("Failed to initialize summarizer")
	}

	// Initialize RAG service if configured.
	var ragSvc *rag.Service
	if cfg.RAGEnabled() {
		llmCfg := openai.DefaultConfig(cfg.OpenRouterKey)
		llmCfg.BaseURL = cfg.OpenRouterURL
		llmCfg.HTTPClient = &http.Client{
			Timeout:   120 * time.Second,
			Transport: &http.Transport{Proxy: http.ProxyFromEnvironment},
		}
		llmClient := openai.NewClientWithConfig(llmCfg)

		ragSvc, err = rag.New(cfg, llmClient, cfg.Model)
		if err != nil {
			logger.Fatal().Err(err).Msg("Failed to initialize RAG service")
		}
		defer func() { _ = ragSvc.Close() }()
		logger.Info().Str("qdrant_addr", cfg.QdrantAddr).Str("collection", cfg.QdrantCollection).Msg("RAG service initialized")
	}

	// Backfill mode: import Telegram export, then exit.
	if *backfillPath != "" {
		if ragSvc == nil {
			logger.Fatal().Msg("RAG is not configured (EMBEDDING_URL is empty); cannot backfill")
		}
		if *backfillGroupID == 0 {
			logger.Fatal().Msg("--group-id is required for backfill")
		}

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
		defer cancel()

		saltCtx, saltCancel := context.WithTimeout(context.Background(), 10*time.Second)
		salt, saltErr := database.GetUserHashSalt(saltCtx)
		saltCancel()
		if saltErr != nil {
			logger.Fatal().Err(saltErr).Msg("Failed to load user hash salt")
		}

		if err := ragSvc.Backfill(ctx, *backfillPath, *backfillGroupID, *backfillReset, salt); err != nil {
			logger.Fatal().Err(err).Msg("Backfill failed")
		}
		logger.Info().Msg("Backfill complete")
		return
	}

	tgBot, err := bot.NewBot(cfg, database, sum, m, ragSvc)
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

	tgBot.FlushMetrics()

	logger.Info().Msg("Bot shutdown complete")
}
