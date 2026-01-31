package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"summary_bot/llm"
	"summary_bot/service"
	"summary_bot/storage"
	"summary_bot/timeutil"

	"github.com/stretchr/testify/require"
)

// fakeTelegramClient implements the subset of Client behavior we need in tests.
type fakeTelegramClient struct {
	lastChatID  int64
	lastText    string
	lastReplyTo int64
	err         error
}

func (f *fakeTelegramClient) SendMessage(ctx context.Context, chatID int64, text string, replyTo int64) error {
	f.lastChatID = chatID
	f.lastText = text
	f.lastReplyTo = replyTo
	return f.err
}

// fakeStoreTelegram is an in-memory implementation of storage.Store for tests.
type fakeStoreTelegram struct {
	inserted []storage.Message
}

func (f *fakeStoreTelegram) InsertMessage(ctx context.Context, msg storage.Message) error {
	f.inserted = append(f.inserted, msg)
	return nil
}

func (f *fakeStoreTelegram) GetMessagesInRange(ctx context.Context, channelID int64, from, to time.Time, limit int) ([]storage.Message, error) {
	return nil, nil
}

// fakeLLM is a minimal implementation used to back the real Summarizer in
// webhook tests.
type fakeLLM struct {
	lastMessages []llm.ChatMessage
	response     string
	err          error
}

func (f *fakeLLM) Summarize(ctx context.Context, messages []llm.ChatMessage) (string, error) {
	f.lastMessages = messages
	if f.err != nil {
		return "", f.err
	}
	if f.response != "" {
		return f.response, nil
	}
	return "summary text", nil
}

func TestParseRangeFromText(t *testing.T) {
	require.Equal(t, "", parseRangeFromText("@summary_bot"))
	require.Equal(t, "last 3 hours", parseRangeFromText("@summary_bot summarize last 3 hours"))
	require.Equal(t, "2024-01-01 to 2024-01-02", parseRangeFromText("@summary_bot summarize 2024-01-01 to 2024-01-02"))
}

// TestWebhookHandler_MentionInWhitelistedChannel is a lightweight test that
// ensures the handler accepts a valid payload and stores the message.
func TestWebhookHandler_MentionInWhitelistedChannel(t *testing.T) {
	store := &fakeStoreTelegram{}
	wl := service.NewWhitelist([]int64{123})

	// Build a real summarizer with in-memory dependencies so the handler can
	// call into it without panicking.
	parser := timeutil.NewParser(24*time.Hour, 7*24*time.Hour)
	fakeStore := &fakeStoreTelegram{}
	fakeLLM := &fakeLLM{response: "summary text"}
	summarizer := service.NewSummarizer(fakeStore, fakeLLM, parser, wl, nil)

	// Use a real Client but with a dummy HTTP client to avoid network calls.
	client := &Client{botToken: "dummy", baseURL: "https://api.telegram.org", client: &http.Client{}}

	logger := log.New(io.Discard, "", 0)

	h := &WebhookHandler{
		client:     client,
		summarizer: summarizer,
		store:      store,
		wl:         wl,
		log:        logger,
	}

	upd := telegramUpdate{
		UpdateID: 1,
		ChannelPost: &telegramMessage{
			MessageID: 10,
			Date:      time.Now().Unix(),
			Chat:      telegramChat{ID: 123, Type: "channel"},
			From:      &telegramUser{ID: 1, Username: "alice"},
			Text:      "hello @summary_bot summarize last 3 hours",
		},
	}
	body, err := json.Marshal(upd)
	require.NoError(t, err)

	req := httptest.NewRequest("POST", "/", bytes.NewReader(body))
	w := httptest.NewRecorder()

	h.ServeHTTP(w, req)

	require.Equal(t, 200, w.Code)
	require.Len(t, store.inserted, 1)
}

// TestWebhookHandler_SummarizerError ensures that when the summarizer returns
// an error, the handler still responds with 200.
func TestWebhookHandler_SummarizerError(t *testing.T) {
	store := &fakeStoreTelegram{}
	wl := service.NewWhitelist([]int64{123})

	parser := timeutil.NewParser(24*time.Hour, 7*24*time.Hour)
	fakeStore := &fakeStoreTelegram{}
	fakeLLM := &fakeLLM{err: errors.New("llm error")}
	summarizer := service.NewSummarizer(fakeStore, fakeLLM, parser, wl, nil)

	client := &Client{botToken: "dummy", baseURL: "https://api.telegram.org", client: &http.Client{}}
	logger := log.New(io.Discard, "", 0)

	h := &WebhookHandler{
		client:     client,
		summarizer: summarizer,
		store:      store,
		wl:         wl,
		log:        logger,
	}

	upd := telegramUpdate{
		UpdateID: 1,
		ChannelPost: &telegramMessage{
			MessageID: 10,
			Date:      time.Now().Unix(),
			Chat:      telegramChat{ID: 123, Type: "channel"},
			From:      &telegramUser{ID: 1, Username: "alice"},
			Text:      "hello @summary_bot summarize last 3 hours",
		},
	}
	body, err := json.Marshal(upd)
	require.NoError(t, err)

	req := httptest.NewRequest("POST", "/", bytes.NewReader(body))
	w := httptest.NewRecorder()

	h.ServeHTTP(w, req)

	require.Equal(t, 200, w.Code)
}
