package summarizer

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sashabaranov/go-openai"
	"telegram_summarize_bot/db"
	"telegram_summarize_bot/metrics"
)

type fakeChatClient struct {
	responses []string
	err       error
	requests  []openai.ChatCompletionRequest
}

func (f *fakeChatClient) CreateChatCompletion(_ context.Context, request openai.ChatCompletionRequest) (openai.ChatCompletionResponse, error) {
	f.requests = append(f.requests, request)
	if f.err != nil {
		return openai.ChatCompletionResponse{}, f.err
	}
	if len(f.responses) == 0 {
		return openai.ChatCompletionResponse{}, nil
	}

	content := f.responses[0]
	f.responses = f.responses[1:]
	return openai.ChatCompletionResponse{
		Choices: []openai.ChatCompletionChoice{
			{
				Message: openai.ChatCompletionMessage{
					Content: content,
				},
			},
		},
	}, nil
}

func TestClusterTopicsSanitizesAssignments(t *testing.T) {
	client := &fakeChatClient{
		responses: []string{
			`{"topics":[{"title":"Релиз","message_indexes":[0,1,1],"message_count":3},{"title":"Оффтоп","message_indexes":[3],"message_count":1}]}`,
		},
	}
	sum := NewWithClient(client, "test-model", metrics.New(), true)

	messages := []db.Message{
		{Text: "Первое", Timestamp: time.Unix(0, 0)},
		{Text: "Второе", Timestamp: time.Unix(60, 0)},
		{Text: "Третье", Timestamp: time.Unix(120, 0)},
		{Text: "Четвертое", Timestamp: time.Unix(180, 0)},
	}

	clusters, err := sum.ClusterTopics(context.Background(), messages, 5)
	if err != nil {
		t.Fatalf("ClusterTopics returned error: %v", err)
	}

	if got, want := len(clusters), 2; got != want {
		t.Fatalf("len(clusters) = %d, want %d", got, want)
	}
	if got := clusters[0].MessageIndexes; len(got) != 2 || got[0] != 0 || got[1] != 1 {
		t.Fatalf("unexpected first cluster indexes: %#v", got)
	}
	if got := clusters[1].MessageIndexes; len(got) != 2 || got[0] != 2 || got[1] != 3 {
		t.Fatalf("unexpected second cluster indexes: %#v", got)
	}
	if got := client.requests[0].MaxTokens; got != clusterMaxTokens {
		t.Fatalf("cluster MaxTokens = %d, want %d", got, clusterMaxTokens)
	}
}

func TestClusterTopicsRejectsInvalidJSON(t *testing.T) {
	sum := NewWithClient(&fakeChatClient{responses: []string{"not-json"}}, "test-model", metrics.New(), true)

	_, err := sum.ClusterTopics(context.Background(), []db.Message{{Text: "msg", Timestamp: time.Unix(0, 0)}}, 5)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "parse topic clusters") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestSummarizeByTopicsUsesConfiguredTokenBudget(t *testing.T) {
	client := &fakeChatClient{
		responses: []string{
			`{"topics":[{"title":"Релиз","message_indexes":[0,1],"message_count":2}]}`,
			`{"tldr":"Обсудили релиз.","topics":[{"title":"Релиз","summary":"Договорились выкатить сегодня.","message_count":2}]}`,
		},
	}
	sum := NewWithClient(client, "test-model", metrics.New(), true)

	summary, err := sum.SummarizeByTopics(context.Background(), []db.Message{
		{Text: "катим релиз", Timestamp: time.Unix(0, 0)},
		{Text: "ок", Timestamp: time.Unix(60, 0)},
	}, 5)
	if err != nil {
		t.Fatalf("SummarizeByTopics returned error: %v", err)
	}

	if summary.TLDR != "Обсудили релиз." {
		t.Fatalf("unexpected TLDR: %q", summary.TLDR)
	}
	if got, want := len(client.requests), 2; got != want {
		t.Fatalf("request count = %d, want %d", got, want)
	}
	if got := client.requests[1].MaxTokens; got != finalMaxTokens {
		t.Fatalf("summary MaxTokens = %d, want %d", got, finalMaxTokens)
	}
}

func TestSummarizeByTopicsPropagatesClientError(t *testing.T) {
	sum := NewWithClient(&fakeChatClient{err: errors.New("boom")}, "test-model", metrics.New(), true)

	_, err := sum.SummarizeByTopics(context.Background(), []db.Message{{Text: "msg", Timestamp: time.Unix(0, 0)}}, 5)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestFormatTelegramSummaryEscapesMarkdown(t *testing.T) {
	formatted := FormatTelegramSummary(&StructuredSummary{
		TLDR: "Итог_1",
		Topics: []TopicSummary{
			{
				Title:   "Релиз_[v1]",
				Summary: "Нужен *фикс* сегодня.",
			},
		},
	})

	if !strings.Contains(formatted, "*TL;DR:* Итог\\_1") {
		t.Fatalf("formatted TLDR missing escape: %q", formatted)
	}
	if !strings.Contains(formatted, "*1. Релиз\\_\\[v1]*") {
		t.Fatalf("formatted title missing escape: %q", formatted)
	}
	if !strings.Contains(formatted, "Нужен \\*фикс\\* сегодня.") {
		t.Fatalf("formatted summary missing escape: %q", formatted)
	}
}
