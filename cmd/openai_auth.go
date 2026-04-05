package cmd

import (
	"github.com/spf13/cobra"
)

var authCmd = &cobra.Command{
	Use:   "auth",
	Short: "OAuth PKCE login flow for OpenAI",
	RunE: func(cmd *cobra.Command, args []string) error {
		return RunAuth(cfg.OAuthClientID, cfg.OAuthTokenDir)
	},
}

var tokenRefreshCmd = &cobra.Command{
	Use:   "token-refresh",
	Short: "Force OAuth token refresh",
	RunE: func(cmd *cobra.Command, args []string) error {
		return RunTokenRefresh(cfg.OAuthClientID, cfg.OAuthTokenDir)
	},
}

func init() {
	openaiCmd.AddCommand(authCmd)
	authCmd.AddCommand(tokenRefreshCmd)
}
