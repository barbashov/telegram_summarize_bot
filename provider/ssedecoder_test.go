package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// codexStreamWithKeepalives mimics the ChatGPT Codex backend: a Responses SSE
// stream that interleaves comment-only keepalive blocks and data-less events
// between the real events. Before the tolerant decoder, the SDK dispatched the
// empty blocks as events and died on `unexpected end of JSON input`.
const codexStreamWithKeepalives = "event: response.created\n" +
	"data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\",\"status\":\"in_progress\"}}\n\n" +
	"event: response.in_progress\n" +
	"data: {\"type\":\"response.in_progress\",\"response\":{\"id\":\"resp_1\",\"status\":\"in_progress\"}}\n\n" +
	": keepalive\n\n" +
	"event: ping\n\n" +
	"data: \n\n" +
	"event: ping\ndata: ping\n\n" +
	"\n" +
	"event: response.output_text.delta\n" +
	"data: {\"type\":\"response.output_text.delta\",\"delta\":\"hel\"}\n\n" +
	": keepalive\n\n" +
	"event: response.output_text.delta\n" +
	"data: {\"type\":\"response.output_text.delta\",\"delta\":\"lo\"}\n\n" +
	"event: response.completed\n" +
	"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"status\":\"completed\"," +
	"\"output\":[{\"type\":\"message\",\"content\":[{\"type\":\"output_text\",\"text\":\"hello\"}]}]," +
	"\"usage\":{\"input_tokens\":10,\"output_tokens\":2,\"total_tokens\":12}}}\n\n"

func newStreamingTestClient(t *testing.T, contentType, body string) *responsesClient {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if contentType == "" {
			// Like the Codex backend: no Content-Type at all (and stop
			// net/http from sniffing one).
			w.Header()["Content-Type"] = nil
		} else {
			w.Header().Set("Content-Type", contentType)
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)

	client, err := NewResponsesClient("test-token", server.URL, 0)
	if err != nil {
		t.Fatalf("NewResponsesClient: %v", err)
	}
	rc, ok := client.(*responsesClient)
	if !ok {
		t.Fatalf("client is %T, want *responsesClient", client)
	}
	// An account ID switches Complete to the streaming (Codex) path.
	rc.setCredentials("test-token", "acct_1")
	return rc
}

func TestStreamingToleratesKeepaliveBlocks(t *testing.T) {
	for _, contentType := range []string{"", "text/event-stream", "text/event-stream; charset=utf-8", "Text/Event-Stream;charset=UTF-8"} {
		t.Run("content-type="+contentType, func(t *testing.T) {
			rc := newStreamingTestClient(t, contentType, codexStreamWithKeepalives)
			resp, err := rc.Complete(context.Background(), CompletionRequest{
				Model:    "gpt-5-codex",
				Messages: []Message{{Role: "user", Content: "hi"}},
			})
			if err != nil {
				t.Fatalf("Complete: %v", err)
			}
			if resp.Content != "hello" {
				t.Errorf("Content = %q, want %q", resp.Content, "hello")
			}
			if resp.Usage.TotalTokens != 12 {
				t.Errorf("TotalTokens = %d, want 12", resp.Usage.TotalTokens)
			}
		})
	}
}

func TestStreamingStillSurfacesErrorEvents(t *testing.T) {
	body := "event: response.created\n" +
		"data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\",\"status\":\"in_progress\"}}\n\n" +
		": keepalive\n\n" +
		"event: error\n" +
		"data: {\"type\":\"error\",\"error\":{\"message\":\"usage limit reached\"}}\n\n"
	rc := newStreamingTestClient(t, "text/event-stream", body)
	_, err := rc.Complete(context.Background(), CompletionRequest{
		Model:    "gpt-5-codex",
		Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatal("expected an error from the error event")
	}
	if !strings.Contains(err.Error(), "usage limit reached") {
		t.Errorf("error = %q, want it to carry the server message", err)
	}
}
