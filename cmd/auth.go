package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"time"

	"telegram_summarize_bot/provider"
)

const (
	// Device authorization endpoints (Codex CLI device flow). No inbound
	// callback or local browser is required, so this works on headless/SSH hosts.
	deviceUserCodeURL = "https://auth.openai.com/api/accounts/deviceauth/usercode" // #nosec G101 -- endpoint URL, not a credential
	deviceTokenURL    = "https://auth.openai.com/api/accounts/deviceauth/token"    // #nosec G101 -- endpoint URL, not a credential
	deviceRedirectURI = "https://auth.openai.com/deviceauth/callback"
	deviceVerifyURL   = "https://auth.openai.com/codex/device"

	// Polling bounds for the device authorization flow.
	deviceMinInterval     = 3 * time.Second
	deviceDefaultInterval = 5 * time.Second
	deviceMaxWait         = 15 * time.Minute
)

// RunAuth performs the OpenAI Codex device authorization flow: it requests a
// device code, prints a verification URL + user code for the operator to open
// on any device, polls until sign-in completes, then exchanges the resulting
// authorization code for tokens and stores them.
func RunAuth(ctx context.Context, clientID, tokenDir string) error {
	httpClient := provider.HTTPClient(15 * time.Second)

	// Step 1: request a device code.
	userCode, deviceAuthID, interval, err := requestDeviceCode(ctx, httpClient, deviceUserCodeURL, clientID)
	if err != nil {
		return fmt.Errorf("request device code: %w", err)
	}

	// Step 2: tell the operator where to go.
	fmt.Printf("\nTo sign in, open this URL on any device:\n\n    %s\n\n", deviceVerifyURL)
	fmt.Printf("and enter this code:\n\n    %s\n\n", userCode)
	fmt.Println("Waiting for sign-in... (press Ctrl+C to cancel)")

	// Step 3: poll until the operator completes sign-in.
	authCode, verifier, err := pollDeviceAuth(ctx, httpClient, deviceTokenURL, deviceAuthID, userCode, interval)
	if err != nil {
		return fmt.Errorf("device authorization: %w", err)
	}

	// Step 4: exchange the authorization code for tokens.
	fmt.Println("Exchanging authorization code for tokens...")
	tokens, err := exchangeCode(clientID, authCode, deviceRedirectURI, verifier)
	if err != nil {
		return fmt.Errorf("token exchange: %w", err)
	}

	// Save tokens
	store := provider.NewTokenStore(tokenDir, clientID)
	if err := store.Save(tokens); err != nil {
		return fmt.Errorf("save tokens: %w", err)
	}

	fmt.Printf("\n✓ Authentication successful! Token saved to %s/openai_tokens.json\n", tokenDir)

	// List available models
	if err := listModels(tokens.AccessToken, tokens.AccountID); err != nil {
		fmt.Printf("\nWarning: could not list models: %v\n", err)
	}

	fmt.Printf("\nAdd to your .env:\n")
	fmt.Printf("  LLM_MODE=oauth\n")
	fmt.Printf("  MODEL=gpt-4o\n")
	fmt.Printf("  OAUTH_CODEX_VERSION=%s\n", provider.CodexClientVersion)

	return nil
}

// RunTokenRefresh forces a token refresh and prints the result.
func RunTokenRefresh(clientID, tokenDir string) error {
	store := provider.NewTokenStore(tokenDir, clientID)
	if err := store.Load(); err != nil {
		return fmt.Errorf("load tokens: %w (run '%s openai auth' first)", err, os.Args[0])
	}

	fmt.Println("Forcing token refresh...")
	if err := store.ForceRefresh(); err != nil {
		return fmt.Errorf("refresh failed: %w", err)
	}

	if _, err := store.GetValidToken(); err != nil {
		return fmt.Errorf("get token after refresh: %w", err)
	}

	fmt.Println("✓ Token refreshed successfully")
	return nil
}

// RunModels lists available OpenAI models using stored OAuth tokens.
func RunModels(clientID, tokenDir string) error {
	store := provider.NewTokenStore(tokenDir, clientID)
	if err := store.Load(); err != nil {
		return fmt.Errorf("load tokens: %w (run '%s openai auth' first)", err, os.Args[0])
	}

	token, err := store.GetValidToken()
	if err != nil {
		return err
	}

	return listModels(token, store.GetAccountID())
}

// RunTest sends a test prompt to the given model via OAuth and prints the response.
func RunTest(ctx context.Context, clientID, tokenDir, model string) error {
	client, err := provider.NewOAuthClient(tokenDir, clientID, os.Getenv("OAUTH_CODEX_VERSION"), 0)
	if err != nil {
		return err
	}

	systemPrompt := "You are a helpful assistant. Keep your answer to 2-3 sentences."
	userPrompt := "Explain why cats always land on their feet, but in the style of a dramatic movie trailer narrator."

	fmt.Printf("Sending test prompt to %s...\n", model)
	fmt.Printf("\nSystem prompt:\n%s\n", systemPrompt)
	fmt.Printf("\nUser prompt:\n%s\n", userPrompt)

	resp, err := client.Complete(ctx, provider.CompletionRequest{
		Model: model,
		Messages: []provider.Message{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: userPrompt},
		},
		MaxTokens:   256,
		Temperature: 0.8,
	})
	if err != nil {
		return fmt.Errorf("completion failed: %w", err)
	}

	fmt.Printf("\n✓ Model %s responded (finish_reason=%s):\n\n%s\n", model, resp.FinishReason, resp.Content)
	return nil
}

// requestDeviceCode asks the device-auth endpoint for a user code. The returned
// interval is the server-suggested poll cadence, clamped to a sane minimum.
func requestDeviceCode(ctx context.Context, client *http.Client, endpoint, clientID string) (userCode, deviceAuthID string, interval time.Duration, err error) {
	body, err := json.Marshal(map[string]string{"client_id": clientID})
	if err != nil {
		return "", "", 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return "", "", 0, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return "", "", 0, err
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", "", 0, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	// interval may arrive as a JSON number or string; json.Number handles both.
	var parsed struct {
		UserCode     string      `json:"user_code"`
		DeviceAuthID string      `json:"device_auth_id"`
		Interval     json.Number `json:"interval"`
	}
	dec := json.NewDecoder(bytes.NewReader(respBody))
	dec.UseNumber()
	if err := dec.Decode(&parsed); err != nil {
		return "", "", 0, fmt.Errorf("parse response: %w (body: %s)", err, string(respBody))
	}
	if parsed.UserCode == "" || parsed.DeviceAuthID == "" {
		return "", "", 0, fmt.Errorf("incomplete device code response: %s", string(respBody))
	}

	interval = deviceDefaultInterval
	if secs, err := parsed.Interval.Int64(); err == nil && secs > 0 {
		interval = time.Duration(secs) * time.Second
	}
	if interval < deviceMinInterval {
		interval = deviceMinInterval
	}

	return parsed.UserCode, parsed.DeviceAuthID, interval, nil
}

// pollDeviceAuth polls the device-auth token endpoint until the operator
// completes sign-in (HTTP 200), returning the server-supplied authorization
// code and PKCE verifier. HTTP 403/404 mean "still pending"; any other status
// is a hard failure. The poll runs until ctx is cancelled or deviceMaxWait.
func pollDeviceAuth(ctx context.Context, client *http.Client, endpoint, deviceAuthID, userCode string, interval time.Duration) (authCode, verifier string, err error) {
	deadline := time.Now().Add(deviceMaxWait)
	body, err := json.Marshal(map[string]string{"device_auth_id": deviceAuthID, "user_code": userCode})
	if err != nil {
		return "", "", err
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return "", "", ctx.Err()
		case <-ticker.C:
		}
		if time.Now().After(deadline) {
			return "", "", fmt.Errorf("timed out after %s waiting for sign-in", deviceMaxWait)
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
		if err != nil {
			return "", "", err
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := client.Do(req)
		if err != nil {
			return "", "", err
		}
		respBody, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()

		switch resp.StatusCode {
		case http.StatusOK:
			var parsed struct {
				AuthorizationCode string `json:"authorization_code"`
				CodeVerifier      string `json:"code_verifier"`
			}
			if err := json.Unmarshal(respBody, &parsed); err != nil {
				return "", "", fmt.Errorf("parse response: %w (body: %s)", err, string(respBody))
			}
			if parsed.AuthorizationCode == "" {
				return "", "", fmt.Errorf("no authorization_code in response: %s", string(respBody))
			}
			return parsed.AuthorizationCode, parsed.CodeVerifier, nil
		case http.StatusForbidden, http.StatusNotFound:
			// Authorization still pending; keep polling.
			continue
		default:
			return "", "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(respBody))
		}
	}
}

func exchangeCode(clientID, code, redirectURI, verifier string) (*provider.OAuthTokens, error) {
	tr, err := provider.PostTokenRequest(url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {clientID},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"code_verifier": {verifier},
	})
	if err != nil {
		return nil, err
	}

	// The device flow may omit id_token (no openid scope requested); the Codex
	// access token is itself a JWT carrying the chatgpt_account_id claim, so
	// fall back to it when id_token yields nothing.
	accountID := provider.ExtractAccountID(tr.IDToken)
	if accountID == "" {
		accountID = provider.ExtractAccountID(tr.AccessToken)
	}

	return &provider.OAuthTokens{
		AccessToken:  tr.AccessToken,
		RefreshToken: tr.RefreshToken,
		IDToken:      tr.IDToken,
		AccountID:    accountID,
		ExpiresAt:    time.Now().Add(time.Duration(tr.ExpiresIn) * time.Second),
	}, nil
}

type modelsResponse struct {
	Models []struct {
		Slug        string `json:"slug"`
		DisplayName string `json:"display_name"`
	} `json:"models"`
}

func listModels(accessToken, accountID string) error {
	req, err := http.NewRequest("GET", provider.ChatGPTCodexBaseURL+"/models?client_version="+provider.CodexClientVersion, http.NoBody)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("version", provider.CodexClientVersion)
	req.Header.Set("originator", provider.CodexOriginator)
	if accountID != "" {
		req.Header.Set(provider.HeaderAccountID, accountID)
	}

	resp, err := provider.HTTPClient(10 * time.Second).Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}

	var models modelsResponse
	if err := json.NewDecoder(resp.Body).Decode(&models); err != nil {
		return err
	}

	if len(models.Models) == 0 {
		fmt.Println("\nNo models available.")
		return nil
	}

	fmt.Println("\nAvailable models:")
	for _, m := range models.Models {
		if m.DisplayName != "" {
			fmt.Printf("  - %s (%s)\n", m.Slug, m.DisplayName)
		} else {
			fmt.Printf("  - %s\n", m.Slug)
		}
	}
	return nil
}
