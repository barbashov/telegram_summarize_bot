package provider

import (
	"context"
	"fmt"
)

type oauthClient struct {
	inner      LLMClient
	tokenStore *TokenStore
}

// NewOAuthClient creates an LLM client that authenticates via OAuth tokens.
// Uses the Responses API under the hood with OAuth token injection.
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

	// Create a responses client with the current token
	inner, err := NewResponsesClient(token, "https://api.openai.com/v1")
	if err != nil {
		return nil, fmt.Errorf("create responses client: %w", err)
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

	// Update the inner client's token
	if rc, ok := c.inner.(*responsesClient); ok {
		rc.token = token
	}

	return c.inner.Complete(ctx, req)
}
