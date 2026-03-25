package rag

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"telegram_summarize_bot/db"
	"telegram_summarize_bot/logger"
)

const backfillBatchSize = 100

// telegramExportMessage represents a single message from Telegram JSON export.
type telegramExportMessage struct {
	ID            int64           `json:"id"`
	Type          string          `json:"type"`
	Date          string          `json:"date"`
	DateUnixtime  string          `json:"date_unixtime"`
	FromID        string          `json:"from_id"`
	Text          json.RawMessage `json:"text"`
	ForwardedFrom string          `json:"forwarded_from"`
	ReplyToMsgID  int64           `json:"reply_to_message_id"`
}

type telegramExport struct {
	Messages []telegramExportMessage `json:"messages"`
}

// ExtractText handles both string and mixed entity array formats of Telegram export text.
func ExtractText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}

	// Try plain string first.
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}

	// Try mixed entity array: [string, {"type":"bold","text":"hello"}, ...]
	var parts []json.RawMessage
	if err := json.Unmarshal(raw, &parts); err != nil {
		return ""
	}

	var sb strings.Builder
	for _, part := range parts {
		var str string
		if err := json.Unmarshal(part, &str); err == nil {
			sb.WriteString(str)
			continue
		}
		var entity struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(part, &entity); err == nil {
			sb.WriteString(entity.Text)
		}
	}
	return sb.String()
}

// ParseFromID extracts numeric user ID from Telegram export from_id (e.g. "user123456" → 123456).
func ParseFromID(fromID string) int64 {
	s := strings.TrimPrefix(fromID, "user")
	s = strings.TrimPrefix(s, "channel")
	s = strings.TrimPrefix(s, "chat")
	id, _ := strconv.ParseInt(s, 10, 64)
	return id
}

// Backfill reads a Telegram JSON export file and imports messages into Qdrant.
func (s *Service) Backfill(ctx context.Context, filePath string, groupID int64, reset bool, userHashSalt []byte) error {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return fmt.Errorf("backfill: read file: %w", err)
	}

	var export telegramExport
	if err := json.Unmarshal(data, &export); err != nil {
		return fmt.Errorf("backfill: parse json: %w", err)
	}

	// Auto-detect dims if needed.
	dims := uint64(s.cfg.EmbeddingDims)
	if dims == 0 {
		vec, err := s.embedder.embedSingle(ctx, "probe")
		if err != nil {
			return fmt.Errorf("backfill: probe embedding: %w", err)
		}
		dims = uint64(len(vec))
	}

	if err := s.store.ensureCollection(ctx, dims); err != nil {
		return fmt.Errorf("backfill: ensure collection: %w", err)
	}

	if reset {
		gidStr := strconv.FormatInt(groupID, 10)
		logger.Info().Int64("group_id", groupID).Msg("backfill: resetting group vectors")
		if err := s.store.deleteByGroup(ctx, gidStr); err != nil {
			return fmt.Errorf("backfill: reset group: %w", err)
		}
	}

	// Filter and prepare messages.
	var messages []Message
	for _, m := range export.Messages {
		if m.Type != "message" {
			continue
		}
		text := ExtractText(m.Text)
		if utf8.RuneCountInString(text) < minMessageRunes {
			continue
		}

		var ts time.Time
		if m.DateUnixtime != "" {
			unix, err := strconv.ParseInt(m.DateUnixtime, 10, 64)
			if err == nil {
				ts = time.Unix(unix, 0)
			}
		}
		if ts.IsZero() && m.Date != "" {
			ts, _ = time.Parse("2006-01-02T15:04:05", m.Date)
		}
		if ts.IsZero() {
			continue
		}

		userID := ParseFromID(m.FromID)
		userHash := ""
		if userID != 0 {
			userHash = db.UserHash(userID, groupID, userHashSalt)
		}

		messages = append(messages, Message{
			GroupID:       groupID,
			UserHash:      userHash,
			Text:          text,
			Timestamp:     ts,
			ForwardedFrom: m.ForwardedFrom,
			TgMessageID:   m.ID,
			ReplyToTgID:   m.ReplyToMsgID,
		})
	}

	logger.Info().
		Int("total_export", len(export.Messages)).
		Int("filtered", len(messages)).
		Msg("backfill: parsed messages")

	// Process in batches.
	for i := 0; i < len(messages); i += backfillBatchSize {
		end := i + backfillBatchSize
		if end > len(messages) {
			end = len(messages)
		}
		batch := messages[i:end]

		if err := s.processBatch(ctx, batch); err != nil {
			return fmt.Errorf("backfill: batch %d-%d: %w", i, end, err)
		}

		logger.Info().
			Int("progress", end).
			Int("total", len(messages)).
			Msg("backfill: batch complete")
	}

	logger.Info().Int("imported", len(messages)).Msg("backfill: complete")
	return nil
}
