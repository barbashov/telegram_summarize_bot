package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
	"telegram_summarize_bot/config"
)

var openaiCmd = &cobra.Command{
	Use:   "openai",
	Short: "OpenAI OAuth and model management",
	PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
		var err error
		cfg, err = config.Load()
		if err != nil {
			return fmt.Errorf("failed to load config: %w", err)
		}
		return nil
	},
}

func init() {
	rootCmd.AddCommand(openaiCmd)
}
