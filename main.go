package main

import (
	"context"
	"database/sql"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"summary_bot/config"
	"summary_bot/llm"
	"summary_bot/service"
	"summary_bot/storage"
	"summary_bot/telegram"
	"summary_bot/timeutil"
)

func main() {
	logger := log.New(os.Stdout, "summary_bot ", log.LstdFlags|log.LUTC|log.Lshortfile)

	cfg, err := config.Load()
	if err != nil {
		logger.Fatalf("failed to load config: %v", err)
	}

	db, err := sql.Open("sqlite3", cfg.DatabasePath)
	if err != nil {
		logger.Fatalf("failed to open database: %v", err)
	}
	defer db.Close()

	if err := storage.InitSchema(db); err != nil {
		logger.Fatalf("failed to init database schema: %v", err)
	}

	store := storage.NewSQLiteStore(db)
	timeParser := timeutil.NewParser(cfg.DefaultHistoryWindow, cfg.MaxHistoryWindow)
	llmClient := llm.NewOpenAIClient(cfg.OpenAIAPIKey, logger)

	whitelist := service.NewWhitelist(cfg.WhitelistedChannels)
	summarizer := service.NewSummarizer(store, llmClient, timeParser, whitelist, logger)

	telegramClient := telegram.NewClient(cfg.TelegramBotToken, cfg.TelegramAPIBaseURL, logger)
	handler := telegram.NewWebhookHandler(telegramClient, summarizer, store, whitelist, logger)

	mux := http.NewServeMux()
	mux.Handle(cfg.WebhookPath, handler)

	server := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		logger.Printf("starting HTTP server on %s", cfg.ListenAddr)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Fatalf("http server error: %v", err)
		}
	}()

	// Graceful shutdown
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := server.Shutdown(ctx); err != nil {
		logger.Printf("http server shutdown error: %v", err)
	}

	logger.Println("shutdown complete")
}
