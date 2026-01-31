package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"summary_bot/llm"
	"summary_bot/storage"
	"summary_bot/timeutil"

	"github.com/stretchr/testify/require"
)

type fakeStore struct {
	msgs []storage.Message
}

func (f *fakeStore) InsertMessage(ctx context.Context, msg storage.Message) error {
	f.msgs = append(f.msgs, msg)
	return nil
}

func (f *fakeStore) GetMessagesInRange(ctx context.Context, channelID int64, from, to time.Time, limit int) ([]storage.Message, error) {
	var out []storage.Message
	for _, m := range f.msgs {
		if m.ChannelID == channelID && !m.Timestamp.Before(from) && m.Timestamp.Before(to) {
			out = append(out, m)
		}
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

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
	return f.response, nil
}

func TestWhitelist_IsAllowed(t *testing.T) {
	wl := NewWhitelist([]int64{1, 2, 3})
	require.True(t, wl.IsAllowed(1))
	require.True(t, wl.IsAllowed(3))
	require.False(t, wl.IsAllowed(4))
}

func TestSummarizer_ChannelNotAllowed(t *testing.T) {
	store := &fakeStore{}
	llmClient := &fakeLLM{}
	parser := timeutil.NewParser(24*time.Hour, 7*24*time.Hour)
	wl := NewWhitelist([]int64{1})

	s := NewSummarizer(store, llmClient, parser, wl, nil)
	_, err := s.SummarizeChannel(context.Background(), time.Now(), SummaryRequest{ChannelID: 2})
	require.Error(t, err)
}

func TestSummarizer_NoMessages(t *testing.T) {
	store := &fakeStore{}
	llmClient := &fakeLLM{response: "ignored"}
	parser := timeutil.NewParser(24*time.Hour, 7*24*time.Hour)
	wl := NewWhitelist([]int64{1})

	s := NewSummarizer(store, llmClient, parser, wl, nil)

	now := time.Date(2024, 1, 10, 12, 0, 0, 0, time.UTC)
	res, err := s.SummarizeChannel(context.Background(), now, SummaryRequest{ChannelID: 1})
	require.NoError(t, err)
	require.Equal(t, "No messages found in the requested time range.", res)
}

func TestSummarizer_BuildsHistoryAndCallsLLM(t *testing.T) {
	store := &fakeStore{}
	now := time.Date(2024, 1, 10, 12, 0, 0, 0, time.UTC)
	store.msgs = []storage.Message{
		{ChannelID: 1, SenderID: 42, Username: storage.NullString("alice"), Text: "hello", Timestamp: now.Add(-time.Hour)},
		{ChannelID: 1, SenderID: 43, Username: storage.NullString(""), Text: "world", Timestamp: now.Add(-30 * time.Minute)},
	}

	llmClient := &fakeLLM{response: "summary"}
	parser := timeutil.NewParser(24*time.Hour, 7*24*time.Hour)
	wl := NewWhitelist([]int64{1})

	s := NewSummarizer(store, llmClient, parser, wl, nil)

	res, err := s.SummarizeChannel(context.Background(), now, SummaryRequest{ChannelID: 1})
	require.NoError(t, err)
	require.Equal(t, "summary", res)

	require.Len(t, llmClient.lastMessages, 1)
	require.Equal(t, "user", llmClient.lastMessages[0].Role)
	require.Contains(t, llmClient.lastMessages[0].Content, "Summarize the following Telegram channel history")
	require.Contains(t, llmClient.lastMessages[0].Content, "alice")
	require.Contains(t, llmClient.lastMessages[0].Content, "hello")
	require.Contains(t, llmClient.lastMessages[0].Content, "user-43")
}

func TestSummarizer_LLMErrorPropagated(t *testing.T) {
	store := &fakeStore{}
	now := time.Date(2024, 1, 10, 12, 0, 0, 0, time.UTC)
	store.msgs = []storage.Message{{ChannelID: 1, SenderID: 1, Text: "x", Timestamp: time.Date(2024, 1, 10, 11, 0, 0, 0, time.UTC)}}

	llmClient := &fakeLLM{err: errors.New("llm fail")}
	parser := timeutil.NewParser(24*time.Hour, 7*24*time.Hour)
	wl := NewWhitelist([]int64{1})

	s := NewSummarizer(store, llmClient, parser, wl, nil)

	_, err := s.SummarizeChannel(context.Background(), now, SummaryRequest{ChannelID: 1})
	require.Error(t, err)
}
