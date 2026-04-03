package provider

import (
	"context"
	"fmt"
)

const chatGPTCodexBaseURL = "https://chatgpt.com/backend-api/codex"

type oauthClient struct {
	inner      LLMClient
	tokenStore *TokenStore
}

// NewOAuthClient creates an LLM client that authenticates via OAuth tokens.
// Uses the ChatGPT backend Codex API with the OAuth access_token and ChatGPT-Account-ID header.
func NewOAuthClient(tokenDir, clientID string) (LLMClient, error) {
	store := NewTokenStore(tokenDir, clientID)
	if err := store.Load(); err != nil {
		return nil, fmt.Errorf("load OAuth tokens: %w (run '%s openai auth' first)", err, "bot")
	}

	// Get initial token to verify it's valid
	token, err := store.GetValidToken()
	if err != nil {
		return nil, err
	}

	inner, err := newResponsesClient(token, chatGPTCodexBaseURL, false)
	if err != nil {
		return nil, fmt.Errorf("create responses client: %w", err)
	}
	if rc, ok := inner.(*responsesClient); ok {
		rc.accountID = store.GetAccountID()
	}

	return &oauthClient{
		inner:      inner,
		tokenStore: store,
	}, nil
}

func (c *oauthClient) Complete(ctx context.Context, req CompletionRequest) (CompletionResponse, error) {
	// Get a valid token (auto-refreshes if needed)
	token, err := c.tokenStore.GetValidToken()
	if err != nil {
		return CompletionResponse{}, fmt.Errorf("get OAuth token: %w", err)
	}

	// Update token and account ID on the inner responses client.
	if rc, ok := c.inner.(*responsesClient); ok {
		rc.token = token
		rc.accountID = c.tokenStore.GetAccountID()
	}

	return c.inner.Complete(ctx, req)
}
