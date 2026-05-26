package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"
	"telegram_summarize_bot/logger"
)

// LLMMode selects which LLM API backend to use.
type LLMMode string

const (
	LLMModeCompletions LLMMode = "completions" // OpenAI Chat Completions API (default)
	LLMModeResponses   LLMMode = "responses"   // OpenAI Responses API
	LLMModeOAuth       LLMMode = "oauth"       // OpenAI Codex subscription via OAuth
)

const defaultOAuthClientID = "app_EMoamEEZ73f0CkXaXp7hrann" // Codex CLI well-known client ID
const defaultOAuthCodexVersion = "0.124.0"

// VisionEnabled selects whether the bot tries to describe images during summarization.
type VisionEnabled string

const (
	VisionEnabledAuto  VisionEnabled = "auto"  // capability-detect from model name
	VisionEnabledTrue  VisionEnabled = "true"  // force on
	VisionEnabledFalse VisionEnabled = "false" // force off
)

type Config struct {
	BotToken                 string
	LLMMode                  LLMMode
	LLMToken                 string
	LLMEndpoint              string
	Model                    string
	SummaryHours             int
	RetentionDays            int
	MaxMessages              int
	TopicMax                 int
	RateLimitSec             int
	DBPath                   string
	AllowedGroups            []int64
	AdminUserIDs             []int64
	DailySummaryHour         int
	ReplyThreads             bool
	ReplyThreadContextDepth  int
	URLMaxChars              int
	ReplyMinChars            int
	ReplyChainMaxDepth       int
	ReplyChainMaxLinks       int
	ReplyChainMaxImages      int
	OAuthTokenDir            string
	OAuthClientID            string
	OAuthCodexVersion        string
	VisionEnabled            VisionEnabled
	VisionSteeringEnabled    bool
	VisionModel              string // empty => use Model
	ImageCacheDays           int
	ImageMaxBytes            int
	ImageDescribeConcurrency int
	ImageDescribeTimeoutSec  int
	LLMHTTPTimeoutSec        int
	CodexQuotaTTLSec         int
	ModelContextTokens       int // optional override for context-window utilization; 0 => auto
}

func Load() (*Config, error) {
	_ = godotenv.Load()

	botToken := os.Getenv("BOT_TOKEN")
	if botToken == "" {
		return nil, &ConfigError{Field: "BOT_TOKEN"}
	}

	llmMode := LLMMode(strings.TrimSpace(strings.ToLower(os.Getenv("LLM_MODE"))))
	if llmMode == "" {
		llmMode = LLMModeCompletions
	}

	// LLM_TOKEN with fallback to deprecated OPENROUTER_API_KEY
	llmToken := os.Getenv("LLM_TOKEN")
	if llmToken == "" {
		if legacy := os.Getenv("OPENROUTER_API_KEY"); legacy != "" {
			logger.Warn().Msg("DEPRECATED: OPENROUTER_API_KEY is deprecated, use LLM_TOKEN instead")
			llmToken = legacy
		}
	}

	// LLM_ENDPOINT with fallback to deprecated OPENROUTER_URL
	llmEndpoint := os.Getenv("LLM_ENDPOINT")
	if llmEndpoint == "" {
		if legacy := os.Getenv("OPENROUTER_URL"); legacy != "" {
			logger.Warn().Msg("DEPRECATED: OPENROUTER_URL is deprecated, use LLM_ENDPOINT instead")
			llmEndpoint = legacy
		}
	}

	// Validate and set defaults based on mode
	switch llmMode {
	case LLMModeCompletions:
		if llmToken == "" {
			return nil, &ConfigError{Field: "LLM_TOKEN (or OPENROUTER_API_KEY)"}
		}
		if llmEndpoint == "" {
			llmEndpoint = "https://openrouter.ai/api/v1"
		}
	case LLMModeResponses:
		if llmToken == "" {
			return nil, &ConfigError{Field: "LLM_TOKEN"}
		}
		if llmEndpoint == "" {
			llmEndpoint = "https://api.openai.com/v1"
		}
	case LLMModeOAuth:
		if llmEndpoint == "" {
			llmEndpoint = "https://api.openai.com/v1"
		}
	default:
		return nil, fmt.Errorf("config: unknown LLM_MODE: %q (valid: completions, responses, oauth)", llmMode)
	}

	model := os.Getenv("MODEL")
	if model == "" {
		model = "meta-llama/llama-3.3-70b-instruct"
	}

	dbPath := os.Getenv("DB_PATH")
	if dbPath == "" {
		dbPath = "./data/bot.db"
	}

	oauthTokenDir := os.Getenv("OAUTH_TOKEN_DIR")
	if oauthTokenDir == "" {
		oauthTokenDir = "./data"
	}

	oauthClientID := os.Getenv("OAUTH_CLIENT_ID")
	if oauthClientID == "" {
		oauthClientID = defaultOAuthClientID
	}

	oauthCodexVersion := strings.TrimSpace(os.Getenv("OAUTH_CODEX_VERSION"))
	if oauthCodexVersion == "" {
		oauthCodexVersion = defaultOAuthCodexVersion
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

	visionEnabled := VisionEnabledAuto
	switch strings.TrimSpace(strings.ToLower(os.Getenv("VISION_ENABLED"))) {
	case "true", "1", "yes", "on":
		visionEnabled = VisionEnabledTrue
	case "false", "0", "no", "off":
		visionEnabled = VisionEnabledFalse
	}

	visionSteering := true
	if v := strings.TrimSpace(strings.ToLower(os.Getenv("VISION_STEERING"))); v == "false" || v == "0" || v == "no" || v == "off" {
		visionSteering = false
	}

	return &Config{
		BotToken:                 botToken,
		LLMMode:                  llmMode,
		LLMToken:                 llmToken,
		LLMEndpoint:              llmEndpoint,
		Model:                    model,
		SummaryHours:             envIntOr("SUMMARY_HOURS", 24),
		RetentionDays:            envIntOr("RETENTION_DAYS", 7),
		MaxMessages:              envIntOr("MAX_MESSAGES", 250),
		TopicMax:                 envIntOr("TOPIC_MAX", 5),
		RateLimitSec:             envIntOr("RATE_LIMIT_SEC", 60),
		DBPath:                   dbPath,
		AllowedGroups:            allowedGroups,
		AdminUserIDs:             adminUserIDs,
		DailySummaryHour:         dailySummaryHour,
		ReplyThreads:             replyThreads,
		ReplyThreadContextDepth:  envIntOr("REPLY_THREAD_CONTEXT_DEPTH", 3),
		URLMaxChars:              envIntOr("URL_MAX_CHARS", 64000),
		ReplyMinChars:            envIntOr("REPLY_SUMMARIZE_MIN_CHARS", 1000),
		ReplyChainMaxDepth:       envIntOr("REPLY_CHAIN_MAX_DEPTH", 25),
		ReplyChainMaxLinks:       envIntOr("REPLY_CHAIN_MAX_LINKS", 5),
		ReplyChainMaxImages:      envIntOr("REPLY_CHAIN_MAX_IMAGES", 8),
		OAuthTokenDir:            oauthTokenDir,
		OAuthClientID:            oauthClientID,
		OAuthCodexVersion:        oauthCodexVersion,
		VisionEnabled:            visionEnabled,
		VisionSteeringEnabled:    visionSteering,
		VisionModel:              strings.TrimSpace(os.Getenv("VISION_MODEL")),
		ImageCacheDays:           envIntOr("IMAGE_CACHE_DAYS", 90),
		ImageMaxBytes:            envIntOr("IMAGE_MAX_BYTES", 5_000_000),
		ImageDescribeConcurrency: envIntOr("IMAGE_DESCRIBE_CONCURRENCY", 4),
		ImageDescribeTimeoutSec:  envIntOr("IMAGE_DESCRIBE_TIMEOUT_SEC", 60),
		LLMHTTPTimeoutSec:        envIntOr("LLM_HTTP_TIMEOUT_SEC", 180),
		CodexQuotaTTLSec:         envIntOr("CODEX_QUOTA_TTL_SEC", 900),
		ModelContextTokens:       envIntOr("MODEL_CONTEXT_TOKENS", 0),
	}, nil
}

// CodexQuotaTTL is how long a cached Codex quota snapshot is considered fresh
// before /usage attempts a live refresh.
func (c *Config) CodexQuotaTTL() time.Duration {
	return time.Duration(c.CodexQuotaTTLSec) * time.Second
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

// ImageCacheDuration is the retention window for cached image descriptions.
func (c *Config) ImageCacheDuration() time.Duration {
	return time.Duration(c.ImageCacheDays) * 24 * time.Hour
}

// ImageDescribeTimeout is the per-image vision-call timeout.
func (c *Config) ImageDescribeTimeout() time.Duration {
	return time.Duration(c.ImageDescribeTimeoutSec) * time.Second
}

// LLMHTTPTimeout is the HTTP client timeout applied to every LLM request
// (cluster, summary, vision). Bumping it gives slow vision-enriched prompts
// breathing room before the retry cycle kicks in.
func (c *Config) LLMHTTPTimeout() time.Duration {
	return time.Duration(c.LLMHTTPTimeoutSec) * time.Second
}

// VisionModelOrDefault returns VisionModel if set, otherwise Model.
func (c *Config) VisionModelOrDefault() string {
	if c.VisionModel != "" {
		return c.VisionModel
	}
	return c.Model
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
