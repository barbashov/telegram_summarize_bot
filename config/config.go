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
}

func Load() (*Config, error) {
	_ = godotenv.Load()

	summaryHours := 24
	if v := os.Getenv("SUMMARY_HOURS"); v != "" {
		if parsed, err := strconv.Atoi(v); err == nil && parsed > 0 {
			summaryHours = parsed
		}
	}

	retentionDays := 7
	if v := os.Getenv("RETENTION_DAYS"); v != "" {
		if parsed, err := strconv.Atoi(v); err == nil && parsed > 0 {
			retentionDays = parsed
		}
	}

	maxMessages := 250
	if v := os.Getenv("MAX_MESSAGES"); v != "" {
		if parsed, err := strconv.Atoi(v); err == nil && parsed > 0 {
			maxMessages = parsed
		}
	}

	topicMax := 5
	if v := os.Getenv("TOPIC_MAX"); v != "" {
		if parsed, err := strconv.Atoi(v); err == nil && parsed > 0 {
			topicMax = parsed
		}
	}

	rateLimitSec := 60
	if v := os.Getenv("RATE_LIMIT_SEC"); v != "" {
		if parsed, err := strconv.Atoi(v); err == nil && parsed > 0 {
			rateLimitSec = parsed
		}
	}

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

	return &Config{
		BotToken:         botToken,
		OpenRouterKey:    openRouterKey,
		SummaryHours:     summaryHours,
		RetentionDays:    retentionDays,
		MaxMessages:      maxMessages,
		TopicMax:         topicMax,
		RateLimitSec:     rateLimitSec,
		OpenRouterURL:    openRouterURL,
		Model:            model,
		DBPath:           dbPath,
		AllowedGroups:    allowedGroups,
		AdminUserIDs:     adminUserIDs,
		DailySummaryHour: dailySummaryHour,
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

func (c *Config) IsAdminUser(userID int64) bool {
	for _, id := range c.AdminUserIDs {
		if id == userID {
			return true
		}
	}
	return false
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
