package cmd

import (
	"github.com/spf13/cobra"
)

var modelsCmd = &cobra.Command{
	Use:   "models",
	Short: "List available OpenAI models",
	RunE: func(cmd *cobra.Command, args []string) error {
		return RunModels(cfg.OAuthClientID, cfg.OAuthTokenDir)
	},
}

func init() {
	openaiCmd.AddCommand(modelsCmd)
}
