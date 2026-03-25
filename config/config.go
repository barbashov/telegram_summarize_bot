package config

import (
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"
)

type Config struct {
	BotToken         string
	OpenRouterKey    string
	SummaryHours     int
	RetentionDays    int
	MaxMessages      int
	TopicMax         int
	RateLimitSec     int
	OpenRouterURL    string
	Model            string
	DBPath           string
	AllowedGroups    []int64
	AdminUserIDs     []int64
	DailySummaryHour int
	ReplyThreads     bool
	URLMaxChars      int

	// RAG (optional — disabled when EmbeddingURL is empty)
	EmbeddingModel   string
	EmbeddingURL     string
	EmbeddingAPIKey  string
	EmbeddingDims    int
	QdrantAddr       string
	QdrantCollection string
	RAGTopK          int
	RAGContextWindow int
}

func Load() (*Config, error) {
	_ = godotenv.Load()

	botToken := os.Getenv("BOT_TOKEN")
	openRouterKey := os.Getenv("OPENROUTER_API_KEY")
	openRouterURL := os.Getenv("OPENROUTER_URL")
	model := os.Getenv("MODEL")
	dbPath := os.Getenv("DB_PATH")

	if botToken == "" {
		return nil, &ConfigError{Field: "BOT_TOKEN"}
	}
	if openRouterKey == "" {
		return nil, &ConfigError{Field: "OPENROUTER_API_KEY"}
	}

	if openRouterURL == "" {
		openRouterURL = "https://openrouter.ai/api/v1"
	}
	if model == "" {
		model = "meta-llama/llama-3.3-70b-instruct"
	}
	if dbPath == "" {
		dbPath = "./data/bot.db"
	}

	allowedGroups := parseIDList(os.Getenv("ALLOWED_GROUPS"))
	adminUserIDsRaw := os.Getenv("ADMIN_USER_IDS")
	if adminUserIDsRaw == "" {
		adminUserIDsRaw = os.Getenv("ALERT_USER_IDS")
	}
	adminUserIDs := parseIDList(adminUserIDsRaw)

	dailySummaryHour := 7
	if v := os.Getenv("DAILY_SUMMARY_HOUR"); v != "" {
		if parsed, err := strconv.Atoi(v); err == nil && parsed >= 0 && parsed <= 23 {
			dailySummaryHour = parsed
		}
	}

	replyThreads := true
	if v := strings.TrimSpace(strings.ToLower(os.Getenv("REPLY_THREADS"))); v == "false" || v == "0" {
		replyThreads = false
	}

	qdrantAddr := os.Getenv("QDRANT_ADDR")
	if qdrantAddr == "" {
		qdrantAddr = "localhost:6334"
	}
	qdrantCollection := os.Getenv("QDRANT_COLLECTION")
	if qdrantCollection == "" {
		qdrantCollection = "messages"
	}

	return &Config{
		BotToken:         botToken,
		OpenRouterKey:    openRouterKey,
		SummaryHours:     envIntOr("SUMMARY_HOURS", 24),
		RetentionDays:    envIntOr("RETENTION_DAYS", 7),
		MaxMessages:      envIntOr("MAX_MESSAGES", 250),
		TopicMax:         envIntOr("TOPIC_MAX", 5),
		RateLimitSec:     envIntOr("RATE_LIMIT_SEC", 60),
		OpenRouterURL:    openRouterURL,
		Model:            model,
		DBPath:           dbPath,
		AllowedGroups:    allowedGroups,
		AdminUserIDs:     adminUserIDs,
		DailySummaryHour: dailySummaryHour,
		ReplyThreads:     replyThreads,
		URLMaxChars:      envIntOr("URL_MAX_CHARS", 64000),

		EmbeddingModel:   os.Getenv("EMBEDDING_MODEL"),
		EmbeddingURL:     os.Getenv("EMBEDDING_URL"),
		EmbeddingAPIKey:  os.Getenv("EMBEDDING_API_KEY"),
		EmbeddingDims:    envIntOrZero("EMBEDDING_DIMS"),
		QdrantAddr:       qdrantAddr,
		QdrantCollection: qdrantCollection,
		RAGTopK:          envIntOr("RAG_TOP_K", 10),
		RAGContextWindow: envIntOr("RAG_CONTEXT_WINDOW", 300),
	}, nil
}

type ConfigError struct {
	Field string
}

func (e *ConfigError) Error() string {
	return "config: missing required field: " + e.Field
}

func (c *Config) SummaryDuration() time.Duration {
	return time.Duration(c.SummaryHours) * time.Hour
}

func (c *Config) RetentionDuration() time.Duration {
	return time.Duration(c.RetentionDays) * 24 * time.Hour
}

func (c *Config) RAGEnabled() bool {
	return c.EmbeddingURL != ""
}

func (c *Config) IsAdminUser(userID int64) bool {
	for _, id := range c.AdminUserIDs {
		if id == userID {
			return true
		}
	}
	return false
}

func envIntOr(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if parsed, err := strconv.Atoi(v); err == nil && parsed > 0 {
			return parsed
		}
	}
	return def
}

func envIntOrZero(key string) int {
	if v := os.Getenv(key); v != "" {
		if parsed, err := strconv.Atoi(v); err == nil {
			return parsed
		}
	}
	return 0
}

func parseIDList(value string) []int64 {
	if value == "" {
		return nil
	}

	var admins []int64
	parts := strings.Split(value, ",")
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if id, err := strconv.ParseInt(part, 10, 64); err == nil {
			admins = append(admins, id)
		}
	}
	return admins
}
