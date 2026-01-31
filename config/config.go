package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds all runtime configuration loaded from environment variables.
// This is intentionally simple and uses only the standard library so the
// service can run as a single binary with minimal dependencies.
type Config struct {
	TelegramBotToken    string
	TelegramAPIBaseURL  string
	OpenAIAPIKey        string
	OpenAIAPIBaseURL    string
	WhitelistedChannels []int64

	DefaultHistoryWindow time.Duration
	MaxHistoryWindow     time.Duration

	ListenAddr  string
	WebhookPath string

	DatabasePath string
}

// Load reads configuration from environment variables and applies sensible
// defaults where possible. It performs validation and returns an error if
// required configuration is missing or invalid.
func Load() (*Config, error) {
	cfg := &Config{}

	cfg.TelegramBotToken = strings.TrimSpace(os.Getenv("TELEGRAM_BOT_TOKEN"))
	if cfg.TelegramBotToken == "" {
		return nil, fmt.Errorf("TELEGRAM_BOT_TOKEN is required")
	}

	cfg.OpenAIAPIKey = strings.TrimSpace(os.Getenv("OPENAI_API_KEY"))
	if cfg.OpenAIAPIKey == "" {
		return nil, fmt.Errorf("OPENAI_API_KEY is required")
	}

	cfg.OpenAIAPIBaseURL = strings.TrimSpace(os.Getenv("OPENAI_API_BASE_URL"))
	if cfg.OpenAIAPIBaseURL == "" {
		cfg.OpenAIAPIBaseURL = "https://api.openai.com/v1"
	}

	cfg.TelegramAPIBaseURL = strings.TrimSpace(os.Getenv("TELEGRAM_API_BASE_URL"))
	if cfg.TelegramAPIBaseURL == "" {
		cfg.TelegramAPIBaseURL = "https://api.telegram.org"
	}

	whitelistRaw := strings.TrimSpace(os.Getenv("WHITELISTED_CHANNELS"))
	if whitelistRaw != "" {
		parts := strings.Split(whitelistRaw, ",")
		for _, p := range parts {
			p = strings.TrimSpace(p)
			if p == "" {
				continue
			}
			id, err := strconv.ParseInt(p, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("invalid channel id %q in WHITELISTED_CHANNELS: %w", p, err)
			}
			cfg.WhitelistedChannels = append(cfg.WhitelistedChannels, id)
		}
	}

	defaultWindowStr := strings.TrimSpace(os.Getenv("DEFAULT_HISTORY_WINDOW"))
	if defaultWindowStr == "" {
		cfg.DefaultHistoryWindow = 24 * time.Hour
	} else {
		d, err := time.ParseDuration(defaultWindowStr)
		if err != nil {
			return nil, fmt.Errorf("invalid DEFAULT_HISTORY_WINDOW: %w", err)
		}
		cfg.DefaultHistoryWindow = d
	}

	maxWindowStr := strings.TrimSpace(os.Getenv("MAX_HISTORY_WINDOW"))
	if maxWindowStr == "" {
		cfg.MaxHistoryWindow = 7 * 24 * time.Hour
	} else {
		d, err := time.ParseDuration(maxWindowStr)
		if err != nil {
			return nil, fmt.Errorf("invalid MAX_HISTORY_WINDOW: %w", err)
		}
		cfg.MaxHistoryWindow = d
	}

	if cfg.MaxHistoryWindow < cfg.DefaultHistoryWindow {
		return nil, fmt.Errorf("MAX_HISTORY_WINDOW must be >= DEFAULT_HISTORY_WINDOW")
	}

	cfg.ListenAddr = strings.TrimSpace(os.Getenv("LISTEN_ADDR"))
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = ":8080"
	}

	cfg.WebhookPath = strings.TrimSpace(os.Getenv("WEBHOOK_PATH"))
	if cfg.WebhookPath == "" {
		cfg.WebhookPath = "/telegram/webhook"
	}

	cfg.DatabasePath = strings.TrimSpace(os.Getenv("DATABASE_PATH"))
	if cfg.DatabasePath == "" {
		cfg.DatabasePath = "summary_bot.db"
	}

	return cfg, nil
}
