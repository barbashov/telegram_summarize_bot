package cmd

import (
	"github.com/spf13/cobra"
)

var testModelCmd = &cobra.Command{
	Use:   "test MODEL_NAME",
	Short: "Send a test prompt to a model",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return RunTest(cfg.OAuthClientID, cfg.OAuthTokenDir, args[0])
	},
}

func init() {
	openaiCmd.AddCommand(testModelCmd)
}
