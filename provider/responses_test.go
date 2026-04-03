package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	oaierr "github.com/openai/openai-go"
)

func TestResponsesClientComplete(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Errorf("expected POST, got %s", r.Method)
		}

		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request: %v", err)
		}

		// Verify instructions field (from system message)
		if instr, ok := body["instructions"].(string); !ok || instr != "be helpful" {
			t.Errorf("instructions = %v, want 'be helpful'", body["instructions"])
		}
		// Verify model
		if model, ok := body["model"].(string); !ok || model != "gpt-4o" {
			t.Errorf("model = %v, want 'gpt-4o'", body["model"])
		}

		resp := map[string]any{
			"id":     "resp_123",
			"status": "completed",
			"output": []map[string]any{
				{
					"type": "message",
					"content": []map[string]any{
						{"type": "output_text", "text": "test response"},
					},
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	client, err := NewResponsesClient("test-token", server.URL)
	if err != nil {
		t.Fatalf("NewResponsesClient: %v", err)
	}

	resp, err := client.Complete(context.Background(), CompletionRequest{
		Model: "gpt-4o",
		Messages: []Message{
			{Role: "system", Content: "be helpful"},
			{Role: "user", Content: "hello"},
		},
		MaxTokens:   200,
		Temperature: 0.3,
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Content != "test response" {
		t.Errorf("content = %q, want %q", resp.Content, "test response")
	}
	if resp.FinishReason != "stop" {
		t.Errorf("finish_reason = %q, want %q", resp.FinishReason, "stop")
	}
}

func TestResponsesClientIncompleteStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		resp := map[string]any{
			"id":     "resp_456",
			"status": "incomplete",
			"output": []map[string]any{
				{
					"type": "message",
					"content": []map[string]any{
						{"type": "output_text", "text": "truncated"},
					},
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	client, err := NewResponsesClient("key", server.URL)
	if err != nil {
		t.Fatalf("NewResponsesClient: %v", err)
	}

	resp, err := client.Complete(context.Background(), CompletionRequest{
		Model:    "gpt-4o",
		Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.FinishReason != "length" {
		t.Errorf("finish_reason = %q, want %q", resp.FinishReason, "length")
	}
}

func TestResponsesClientFailedStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		resp := map[string]any{
			"id":     "resp_789",
			"status": "failed",
			"error": map[string]any{
				"code":    "server_error",
				"message": "something broke",
			},
			"output": []any{},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	client, err := NewResponsesClient("key", server.URL)
	if err != nil {
		t.Fatalf("NewResponsesClient: %v", err)
	}

	_, err = client.Complete(context.Background(), CompletionRequest{
		Model:    "gpt-4o",
		Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatal("expected error for failed status")
	}
	apiErr, ok := err.(*APIError)
	if !ok {
		t.Fatalf("expected *APIError, got %T: %v", err, err)
	}
	if apiErr.HTTPStatusCode != 500 {
		t.Errorf("status = %d, want 500", apiErr.HTTPStatusCode)
	}
}

func TestResponsesClientHTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{
				"message": "invalid api key",
				"type":    "invalid_request_error",
			},
		})
	}))
	defer server.Close()

	client, err := NewResponsesClient("bad-key", server.URL)
	if err != nil {
		t.Fatalf("NewResponsesClient: %v", err)
	}

	_, err = client.Complete(context.Background(), CompletionRequest{
		Model:    "gpt-4o",
		Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestWrapResponsesError(t *testing.T) {
	t.Run("wraps oai Error", func(t *testing.T) {
		err := wrapResponsesError(&oaierr.Error{
			StatusCode: 429,
			Message:    "rate limited",
		})
		apiErr, ok := err.(*APIError)
		if !ok {
			t.Fatalf("expected *APIError, got %T", err)
		}
		if apiErr.HTTPStatusCode != 429 {
			t.Errorf("status = %d, want 429", apiErr.HTTPStatusCode)
		}
	})

	t.Run("passes through non-Error", func(t *testing.T) {
		orig := context.Canceled
		err := wrapResponsesError(orig)
		if err != orig {
			t.Errorf("expected original error, got %v", err)
		}
	})
}
