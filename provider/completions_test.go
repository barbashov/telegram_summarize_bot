package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sashabaranov/go-openai"
)

func TestCompletionsClientComplete(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Errorf("expected Bearer test-token, got %s", r.Header.Get("Authorization"))
		}

		var req openai.ChatCompletionRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if req.Model != "test-model" {
			t.Errorf("model = %s, want test-model", req.Model)
		}
		if len(req.Messages) != 2 {
			t.Fatalf("messages count = %d, want 2", len(req.Messages))
		}
		if req.Messages[0].Role != "system" {
			t.Errorf("messages[0].role = %s, want system", req.Messages[0].Role)
		}
		if req.MaxTokens != 100 {
			t.Errorf("max_tokens = %d, want 100", req.MaxTokens)
		}

		resp := openai.ChatCompletionResponse{
			Choices: []openai.ChatCompletionChoice{
				{
					Message:      openai.ChatCompletionMessage{Content: "hello world"},
					FinishReason: "stop",
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	client, err := NewCompletionsClient("test-token", server.URL)
	if err != nil {
		t.Fatalf("NewCompletionsClient: %v", err)
	}

	resp, err := client.Complete(context.Background(), CompletionRequest{
		Model: "test-model",
		Messages: []Message{
			{Role: "system", Content: "be helpful"},
			{Role: "user", Content: "hi"},
		},
		MaxTokens:   100,
		Temperature: 0.5,
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Content != "hello world" {
		t.Errorf("content = %q, want %q", resp.Content, "hello world")
	}
	if resp.FinishReason != "stop" {
		t.Errorf("finish_reason = %q, want %q", resp.FinishReason, "stop")
	}
}

func TestCompletionsClientNoChoices(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		resp := openai.ChatCompletionResponse{Choices: []openai.ChatCompletionChoice{}}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	client, err := NewCompletionsClient("key", server.URL)
	if err != nil {
		t.Fatalf("NewCompletionsClient: %v", err)
	}

	_, err = client.Complete(context.Background(), CompletionRequest{
		Model:    "m",
		Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatal("expected error for no choices")
	}
	apiErr, ok := err.(*APIError)
	if !ok {
		t.Fatalf("expected *APIError, got %T", err)
	}
	if apiErr.HTTPStatusCode != 0 {
		t.Errorf("status = %d, want 0", apiErr.HTTPStatusCode)
	}
}

func TestCompletionsClientAPIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{
				"message": "rate limited",
				"type":    "rate_limit_error",
			},
		})
	}))
	defer server.Close()

	client, err := NewCompletionsClient("key", server.URL)
	if err != nil {
		t.Fatalf("NewCompletionsClient: %v", err)
	}

	_, err = client.Complete(context.Background(), CompletionRequest{
		Model:    "m",
		Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatal("expected error")
	}
	apiErr, ok := err.(*APIError)
	if !ok {
		t.Fatalf("expected *APIError, got %T: %v", err, err)
	}
	if apiErr.HTTPStatusCode != 429 {
		t.Errorf("status = %d, want 429", apiErr.HTTPStatusCode)
	}
}

func TestWrapOpenAIError(t *testing.T) {
	t.Run("wraps APIError", func(t *testing.T) {
		err := wrapOpenAIError(&openai.APIError{
			HTTPStatusCode: 500,
			Message:        "internal",
		})
		apiErr, ok := err.(*APIError)
		if !ok {
			t.Fatalf("expected *APIError, got %T", err)
		}
		if apiErr.HTTPStatusCode != 500 {
			t.Errorf("status = %d, want 500", apiErr.HTTPStatusCode)
		}
	})

	t.Run("passes through non-APIError", func(t *testing.T) {
		orig := context.Canceled
		err := wrapOpenAIError(orig)
		if err != orig {
			t.Errorf("expected original error, got %v", err)
		}
	})
}
