package service

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"summary_bot/llm"
	"summary_bot/storage"
	"summary_bot/timeutil"
)

// Whitelist encapsulates channel access control.
type Whitelist struct {
	allowed map[int64]struct{}
}

// NewWhitelist constructs a whitelist from a slice of channel IDs.
func NewWhitelist(ids []int64) *Whitelist {
	m := make(map[int64]struct{}, len(ids))
	for _, id := range ids {
		m[id] = struct{}{}
	}
	return &Whitelist{allowed: m}
}

// IsAllowed reports whether the given channel is whitelisted.
func (w *Whitelist) IsAllowed(channelID int64) bool {
	if w == nil {
		return false
	}
	_, ok := w.allowed[channelID]
	return ok
}

// Summarizer coordinates fetching messages and calling the LLM to produce a
// summary for a given channel and time range.
type Summarizer struct {
	store     storage.Store
	llm       llm.Client
	timeParse *timeutil.Parser
	wl        *Whitelist
	log       *log.Logger
}

// NewSummarizer constructs a new Summarizer.
func NewSummarizer(store storage.Store, llmClient llm.Client, parser *timeutil.Parser, wl *Whitelist, logger *log.Logger) *Summarizer {
	return &Summarizer{
		store:     store,
		llm:       llmClient,
		timeParse: parser,
		wl:        wl,
		log:       logger,
	}
}

// SummaryRequest describes a summarization request coming from Telegram.
type SummaryRequest struct {
	ChannelID int64
	// RawRange is the user-provided time range expression after the command,
	// e.g. "last 3 hours" or "2024-01-01 to 2024-01-02".
	RawRange string
}

// SummarizeChannel validates access, parses the time range, fetches messages
// from storage, and calls the LLM to obtain a summary.
func (s *Summarizer) SummarizeChannel(ctx context.Context, now time.Time, req SummaryRequest) (string, error) {
	if s.wl == nil || !s.wl.IsAllowed(req.ChannelID) {
		return "", fmt.Errorf("channel not allowed")
	}

	// If no explicit range is provided, default to the last 24 hours.
	rawRange := req.RawRange
	if strings.TrimSpace(rawRange) == "" {
		rawRange = "last 24 hours"
	}

	tr, err := s.timeParse.Parse(now, rawRange)
	if err != nil {
		return "", err
	}

	// Hard limit of messages to avoid exceeding token limits. This is a
	// defensive measure; the exact number can be tuned.
	const maxMessages = 500
	msgs, err := s.store.GetMessagesInRange(ctx, req.ChannelID, tr.From, tr.To, maxMessages)
	if err != nil {
		return "", fmt.Errorf("fetch messages: %w", err)
	}
	if len(msgs) == 0 {
		return "No messages found in the requested time range.", nil
	}

	// Build a compact textual representation of the history. We avoid
	// including any internal metadata beyond what is needed for context.
	var b strings.Builder
	for _, m := range msgs {
		// Format: [HH:MM] username: text
		b.WriteString("[")
		b.WriteString(m.Timestamp.UTC().Format("2006-01-02 15:04"))
		b.WriteString("] ")
		if m.Username.Valid && m.Username.String != "" {
			b.WriteString(m.Username.String)
		} else {
			fmt.Fprintf(&b, "user-%d", m.SenderID)
		}
		b.WriteString(": ")
		b.WriteString(m.Text)
		b.WriteString("\n")
	}

	// SECURITY: We pass the entire history as a single user message. The
	// system prompt in the LLM client ensures that this content is treated as
	// data to summarize, not as instructions.
	chatMsgs := []llm.ChatMessage{
		{
			Role:    "user",
			Content: "Summarize the following Telegram channel history:\n\n" + b.String(),
		},
	}

	// Call the LLM to obtain the summary. If the LLM returns an error, we
	// propagate it to the caller so that upstream components (and tests) can
	// react appropriately instead of silently returning an empty summary.
	summary, err := s.llm.Summarize(ctx, chatMsgs)
	if err != nil {
		return "", fmt.Errorf("llm summarize: %w", err)
	}
	return strings.TrimSpace(summary), nil
}
