package cmd

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"
	"telegram_summarize_bot/config"
	"telegram_summarize_bot/db"
	"telegram_summarize_bot/metrics"
	"telegram_summarize_bot/provider"
	"telegram_summarize_bot/usage"
)

var usageCmd = &cobra.Command{
	Use:   "usage",
	Short: "Show token usage history and Codex account quotas",
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := config.Load()
		if err != nil {
			return fmt.Errorf("failed to load config: %w", err)
		}
		return runUsage(cmd.Context(), c)
	},
}

func init() {
	rootCmd.AddCommand(usageCmd)
}

// runUsage prints the same report as the /usage admin command, reading the
// running bot's database. In OAuth mode it also resolves the Codex quota.
func runUsage(ctx context.Context, cfg *config.Config) error {
	database, err := db.New(cfg.DBPath, metrics.New())
	if err != nil {
		return fmt.Errorf("open database %q: %w", cfg.DBPath, err)
	}
	defer func() { _ = database.Close() }()

	var quota usage.QuotaResult
	if cfg.LLMMode == config.LLMModeOAuth {
		client, err := provider.NewOAuthClient(
			cfg.OAuthTokenDir, cfg.OAuthClientID, cfg.OAuthCodexVersion,
			cfg.LLMHTTPTimeout(), provider.WithRecorder(database),
		)
		if err != nil {
			fmt.Printf("⚠️  Codex quota unavailable: %v\n\n", err)
		} else {
			quota = usage.ResolveCodexQuota(ctx, database, client, cfg.Model, cfg.CodexQuotaTTL())
		}
	}

	report := usage.Build(ctx, database, cfg.Model, cfg.ModelContextTokens, quota)
	fmt.Println(report.Format())
	return nil
}
