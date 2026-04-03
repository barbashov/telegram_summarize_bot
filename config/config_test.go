package config

import (
	"errors"
	"testing"
	"time"
)

// allEnvKeys lists every environment variable that Load() reads.
// Each test that calls Load() should clear them to avoid cross-test leaks.
var allEnvKeys = []string{
	"BOT_TOKEN",
	"LLM_MODE",
	"LLM_TOKEN",
	"LLM_ENDPOINT",
	"OPENROUTER_API_KEY",
	"OPENROUTER_URL",
	"MODEL",
	"DB_PATH",
	"ALLOWED_GROUPS",
	"ADMIN_USER_IDS",
	"ALERT_USER_IDS",
	"SUMMARY_HOURS",
	"RETENTION_DAYS",
	"MAX_MESSAGES",
	"TOPIC_MAX",
	"RATE_LIMIT_SEC",
	"DAILY_SUMMARY_HOUR",
	"REPLY_THREADS",
	"URL_MAX_CHARS",
	"OAUTH_TOKEN_DIR",
	"OAUTH_CLIENT_ID",
}

func clearEnv(t *testing.T) {
	t.Helper()
	for _, k := range allEnvKeys {
		t.Setenv(k, "")
	}
}

func setRequired(t *testing.T) {
	t.Helper()
	t.Setenv("BOT_TOKEN", "test-token")
	t.Setenv("LLM_TOKEN", "test-key")
}

// --- Load validation ---

func TestLoad_MissingBotToken(t *testing.T) {
	clearEnv(t)
	t.Setenv("BOT_TOKEN", "")
	t.Setenv("LLM_TOKEN", "some-key")

	_, err := Load()
	var cfgErr *ConfigError
	if !errors.As(err, &cfgErr) {
		t.Fatalf("expected ConfigError, got %v", err)
	}
	if cfgErr.Field != "BOT_TOKEN" {
		t.Errorf("expected field BOT_TOKEN, got %s", cfgErr.Field)
	}
	if cfgErr.Error() != "config: missing required field: BOT_TOKEN" {
		t.Errorf("unexpected error message: %s", cfgErr.Error())
	}
}

func TestLoad_MissingLLMToken(t *testing.T) {
	clearEnv(t)
	t.Setenv("BOT_TOKEN", "test-token")
	t.Setenv("LLM_TOKEN", "")

	_, err := Load()
	var cfgErr *ConfigError
	if !errors.As(err, &cfgErr) {
		t.Fatalf("expected ConfigError, got %v", err)
	}
	if cfgErr.Field != "LLM_TOKEN (or OPENROUTER_API_KEY)" {
		t.Errorf("expected field LLM_TOKEN, got %s", cfgErr.Field)
	}
}

func TestLoad_LegacyOpenRouterKeyFallback(t *testing.T) {
	clearEnv(t)
	t.Setenv("BOT_TOKEN", "test-token")
	t.Setenv("OPENROUTER_API_KEY", "legacy-key")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.LLMToken != "legacy-key" {
		t.Errorf("LLMToken = %s, want legacy-key", cfg.LLMToken)
	}
}

func TestLoad_LegacyOpenRouterURLFallback(t *testing.T) {
	clearEnv(t)
	setRequired(t)
	t.Setenv("OPENROUTER_URL", "https://custom.api/v1")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.LLMEndpoint != "https://custom.api/v1" {
		t.Errorf("LLMEndpoint = %s, want https://custom.api/v1", cfg.LLMEndpoint)
	}
}

func TestLoad_Defaults(t *testing.T) {
	clearEnv(t)
	setRequired(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	checks := []struct {
		name string
		got  interface{}
		want interface{}
	}{
		{"BotToken", cfg.BotToken, "test-token"},
		{"LLMToken", cfg.LLMToken, "test-key"},
		{"LLMEndpoint", cfg.LLMEndpoint, "https://openrouter.ai/api/v1"},
		{"LLMMode", string(cfg.LLMMode), "completions"},
		{"Model", cfg.Model, "meta-llama/llama-3.3-70b-instruct"},
		{"DBPath", cfg.DBPath, "./data/bot.db"},
		{"SummaryHours", cfg.SummaryHours, 24},
		{"RetentionDays", cfg.RetentionDays, 7},
		{"MaxMessages", cfg.MaxMessages, 250},
		{"TopicMax", cfg.TopicMax, 5},
		{"RateLimitSec", cfg.RateLimitSec, 60},
		{"DailySummaryHour", cfg.DailySummaryHour, 7},
		{"ReplyThreads", cfg.ReplyThreads, true},
		{"URLMaxChars", cfg.URLMaxChars, 64000},
		{"OAuthTokenDir", cfg.OAuthTokenDir, "./data"},
		{"OAuthClientID", cfg.OAuthClientID, defaultOAuthClientID},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}

	if len(cfg.AllowedGroups) != 0 {
		t.Errorf("AllowedGroups = %v, want empty", cfg.AllowedGroups)
	}
	if len(cfg.AdminUserIDs) != 0 {
		t.Errorf("AdminUserIDs = %v, want empty", cfg.AdminUserIDs)
	}
}

func TestLoad_CustomValues(t *testing.T) {
	clearEnv(t)
	t.Setenv("BOT_TOKEN", "custom-token")
	t.Setenv("LLM_TOKEN", "custom-key")
	t.Setenv("LLM_ENDPOINT", "https://custom.api/v1")
	t.Setenv("MODEL", "gpt-4")
	t.Setenv("DB_PATH", "/tmp/test.db")
	t.Setenv("ALLOWED_GROUPS", "-100123,-100456")
	t.Setenv("ADMIN_USER_IDS", "111,222")
	t.Setenv("SUMMARY_HOURS", "12")
	t.Setenv("RETENTION_DAYS", "3")
	t.Setenv("MAX_MESSAGES", "500")
	t.Setenv("TOPIC_MAX", "10")
	t.Setenv("RATE_LIMIT_SEC", "30")
	t.Setenv("DAILY_SUMMARY_HOUR", "15")
	t.Setenv("REPLY_THREADS", "false")
	t.Setenv("URL_MAX_CHARS", "32000")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.BotToken != "custom-token" {
		t.Errorf("BotToken = %s", cfg.BotToken)
	}
	if cfg.LLMToken != "custom-key" {
		t.Errorf("LLMToken = %s", cfg.LLMToken)
	}
	if cfg.LLMEndpoint != "https://custom.api/v1" {
		t.Errorf("LLMEndpoint = %s", cfg.LLMEndpoint)
	}
	if cfg.Model != "gpt-4" {
		t.Errorf("Model = %s", cfg.Model)
	}
	if cfg.DBPath != "/tmp/test.db" {
		t.Errorf("DBPath = %s", cfg.DBPath)
	}
	if cfg.SummaryHours != 12 {
		t.Errorf("SummaryHours = %d", cfg.SummaryHours)
	}
	if cfg.RetentionDays != 3 {
		t.Errorf("RetentionDays = %d", cfg.RetentionDays)
	}
	if cfg.MaxMessages != 500 {
		t.Errorf("MaxMessages = %d", cfg.MaxMessages)
	}
	if cfg.TopicMax != 10 {
		t.Errorf("TopicMax = %d", cfg.TopicMax)
	}
	if cfg.RateLimitSec != 30 {
		t.Errorf("RateLimitSec = %d", cfg.RateLimitSec)
	}
	if cfg.DailySummaryHour != 15 {
		t.Errorf("DailySummaryHour = %d", cfg.DailySummaryHour)
	}
	if cfg.ReplyThreads != false {
		t.Errorf("ReplyThreads = %v", cfg.ReplyThreads)
	}
	if cfg.URLMaxChars != 32000 {
		t.Errorf("URLMaxChars = %d", cfg.URLMaxChars)
	}

	wantGroups := []int64{-100123, -100456}
	if len(cfg.AllowedGroups) != len(wantGroups) {
		t.Fatalf("AllowedGroups length = %d, want %d", len(cfg.AllowedGroups), len(wantGroups))
	}
	for i, v := range wantGroups {
		if cfg.AllowedGroups[i] != v {
			t.Errorf("AllowedGroups[%d] = %d, want %d", i, cfg.AllowedGroups[i], v)
		}
	}

	wantAdmins := []int64{111, 222}
	if len(cfg.AdminUserIDs) != len(wantAdmins) {
		t.Fatalf("AdminUserIDs length = %d, want %d", len(cfg.AdminUserIDs), len(wantAdmins))
	}
	for i, v := range wantAdmins {
		if cfg.AdminUserIDs[i] != v {
			t.Errorf("AdminUserIDs[%d] = %d, want %d", i, cfg.AdminUserIDs[i], v)
		}
	}
}

func TestLoad_ResponsesMode(t *testing.T) {
	clearEnv(t)
	t.Setenv("BOT_TOKEN", "test-token")
	t.Setenv("LLM_MODE", "responses")
	t.Setenv("LLM_TOKEN", "openai-key")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.LLMMode != LLMModeResponses {
		t.Errorf("LLMMode = %s, want responses", cfg.LLMMode)
	}
	if cfg.LLMEndpoint != "https://api.openai.com/v1" {
		t.Errorf("LLMEndpoint = %s, want https://api.openai.com/v1", cfg.LLMEndpoint)
	}
}

func TestLoad_OAuthMode(t *testing.T) {
	clearEnv(t)
	t.Setenv("BOT_TOKEN", "test-token")
	t.Setenv("LLM_MODE", "oauth")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.LLMMode != LLMModeOAuth {
		t.Errorf("LLMMode = %s, want oauth", cfg.LLMMode)
	}
	if cfg.LLMToken != "" {
		t.Errorf("LLMToken = %s, want empty (not required for oauth)", cfg.LLMToken)
	}
	if cfg.OAuthClientID != defaultOAuthClientID {
		t.Errorf("OAuthClientID = %s, want default", cfg.OAuthClientID)
	}
}

func TestLoad_OAuthModeCustomClientID(t *testing.T) {
	clearEnv(t)
	t.Setenv("BOT_TOKEN", "test-token")
	t.Setenv("LLM_MODE", "oauth")
	t.Setenv("OAUTH_CLIENT_ID", "custom-client-id")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.OAuthClientID != "custom-client-id" {
		t.Errorf("OAuthClientID = %s, want custom-client-id", cfg.OAuthClientID)
	}
}

func TestLoad_InvalidMode(t *testing.T) {
	clearEnv(t)
	t.Setenv("BOT_TOKEN", "test-token")
	t.Setenv("LLM_TOKEN", "key")
	t.Setenv("LLM_MODE", "invalid")

	_, err := Load()
	if err == nil {
		t.Fatal("expected error for invalid mode")
	}
}

func TestLoad_AdminUserIDsFallback(t *testing.T) {
	clearEnv(t)
	setRequired(t)
	t.Setenv("ADMIN_USER_IDS", "")
	t.Setenv("ALERT_USER_IDS", "999,888")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	want := []int64{999, 888}
	if len(cfg.AdminUserIDs) != len(want) {
		t.Fatalf("AdminUserIDs length = %d, want %d", len(cfg.AdminUserIDs), len(want))
	}
	for i, v := range want {
		if cfg.AdminUserIDs[i] != v {
			t.Errorf("AdminUserIDs[%d] = %d, want %d", i, cfg.AdminUserIDs[i], v)
		}
	}
}

// --- IsAdminUser ---

func TestIsAdminUser(t *testing.T) {
	cfg := &Config{AdminUserIDs: []int64{100, 200, 300}}

	if !cfg.IsAdminUser(200) {
		t.Error("expected 200 to be admin")
	}
	if cfg.IsAdminUser(999) {
		t.Error("expected 999 to not be admin")
	}
	// Empty list
	empty := &Config{}
	if empty.IsAdminUser(100) {
		t.Error("expected no admins in empty config")
	}
}

// --- Duration helpers ---

func TestSummaryDuration(t *testing.T) {
	cfg := &Config{SummaryHours: 12}
	want := 12 * time.Hour
	if got := cfg.SummaryDuration(); got != want {
		t.Errorf("SummaryDuration() = %v, want %v", got, want)
	}
}

func TestRetentionDuration(t *testing.T) {
	cfg := &Config{RetentionDays: 3}
	want := 3 * 24 * time.Hour
	if got := cfg.RetentionDuration(); got != want {
		t.Errorf("RetentionDuration() = %v, want %v", got, want)
	}
}

// --- parseIDList ---

func TestParseIDList(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  []int64
	}{
		{"empty", "", nil},
		{"single", "42", []int64{42}},
		{"multiple", "1,2,3", []int64{1, 2, 3}},
		{"negative IDs", "-100123,-100456", []int64{-100123, -100456}},
		{"with whitespace", " 10 , 20 , 30 ", []int64{10, 20, 30}},
		{"invalid entries skipped", "1,abc,3", []int64{1, 3}},
		{"all invalid", "foo,bar", nil},
		{"trailing comma", "5,", []int64{5}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseIDList(tt.input)
			if len(got) != len(tt.want) {
				t.Fatalf("parseIDList(%q) length = %d, want %d", tt.input, len(got), len(tt.want))
			}
			for i := range tt.want {
				if got[i] != tt.want[i] {
					t.Errorf("parseIDList(%q)[%d] = %d, want %d", tt.input, i, got[i], tt.want[i])
				}
			}
		})
	}
}

// --- envIntOr ---

func TestEnvIntOr(t *testing.T) {
	t.Run("valid value", func(t *testing.T) {
		t.Setenv("TEST_INT_VAR", "42")
		if got := envIntOr("TEST_INT_VAR", 10); got != 42 {
			t.Errorf("envIntOr = %d, want 42", got)
		}
	})

	t.Run("invalid value returns default", func(t *testing.T) {
		t.Setenv("TEST_INT_VAR", "notanumber")
		if got := envIntOr("TEST_INT_VAR", 10); got != 10 {
			t.Errorf("envIntOr = %d, want 10", got)
		}
	})

	t.Run("missing key returns default", func(t *testing.T) {
		// Use a key that is definitely not set
		if got := envIntOr("DEFINITELY_NOT_SET_12345", 99); got != 99 {
			t.Errorf("envIntOr = %d, want 99", got)
		}
	})

	t.Run("zero returns default", func(t *testing.T) {
		// envIntOr requires parsed > 0, so 0 falls back to default
		t.Setenv("TEST_INT_VAR", "0")
		if got := envIntOr("TEST_INT_VAR", 10); got != 10 {
			t.Errorf("envIntOr = %d, want 10 (zero is not > 0)", got)
		}
	})

	t.Run("negative returns default", func(t *testing.T) {
		t.Setenv("TEST_INT_VAR", "-5")
		if got := envIntOr("TEST_INT_VAR", 10); got != 10 {
			t.Errorf("envIntOr = %d, want 10 (negative is not > 0)", got)
		}
	})
}

// --- ReplyThreads ---

func TestReplyThreads(t *testing.T) {
	tests := []struct {
		name string
		val  string
		want bool
	}{
		{"false disables", "false", false},
		{"0 disables", "0", false},
		{"true keeps enabled", "true", true},
		{"empty keeps enabled", "", true},
		{"random string keeps enabled", "yes", true},
		{"FALSE case insensitive", "FALSE", false},
		{"False mixed case", "False", false},
		{"whitespace around false", " false ", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearEnv(t)
			setRequired(t)
			t.Setenv("REPLY_THREADS", tt.val)

			cfg, err := Load()
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if cfg.ReplyThreads != tt.want {
				t.Errorf("ReplyThreads = %v, want %v (input %q)", cfg.ReplyThreads, tt.want, tt.val)
			}
		})
	}
}
