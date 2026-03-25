package rag

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestExtractText_PlainString(t *testing.T) {
	raw := json.RawMessage(`"hello world"`)
	got := ExtractText(raw)
	if got != "hello world" {
		t.Fatalf("expected 'hello world', got %q", got)
	}
}

func TestExtractText_MixedEntityArray(t *testing.T) {
	raw := json.RawMessage(`["Hello ", {"type":"bold","text":"world"}, "!"]`)
	got := ExtractText(raw)
	if got != "Hello world!" {
		t.Fatalf("expected 'Hello world!', got %q", got)
	}
}

func TestExtractText_Empty(t *testing.T) {
	got := ExtractText(nil)
	if got != "" {
		t.Fatalf("expected empty, got %q", got)
	}
}

func TestExtractText_EmptyArray(t *testing.T) {
	raw := json.RawMessage(`[]`)
	got := ExtractText(raw)
	if got != "" {
		t.Fatalf("expected empty, got %q", got)
	}
}

func TestParseFromID(t *testing.T) {
	tests := []struct {
		input string
		want  int64
	}{
		{"user123456", 123456},
		{"channel789", 789},
		{"chat42", 42},
		{"", 0},
		{"unknown", 0},
	}
	for _, tt := range tests {
		got := ParseFromID(tt.input)
		if got != tt.want {
			t.Errorf("ParseFromID(%q) = %d, want %d", tt.input, got, tt.want)
		}
	}
}

func TestPointID_Deterministic(t *testing.T) {
	id1 := PointID(-1001234567890, 42)
	id2 := PointID(-1001234567890, 42)
	if id1 != id2 {
		t.Fatalf("PointID is not deterministic: %q != %q", id1, id2)
	}

	id3 := PointID(-1001234567890, 43)
	if id1 == id3 {
		t.Fatalf("PointID should differ for different messages")
	}

	id4 := PointID(-1009876543210, 42)
	if id1 == id4 {
		t.Fatalf("PointID should differ for different groups")
	}
}

func TestDeduplicateAndSort(t *testing.T) {
	msgs := []ContextMessage{
		{TgMessageID: "3", Timestamp: 300, Text: "c"},
		{TgMessageID: "1", Timestamp: 100, Text: "a"},
		{TgMessageID: "2", Timestamp: 200, Text: "b"},
		{TgMessageID: "1", Timestamp: 100, Text: "a"}, // duplicate
	}
	got := deduplicateAndSort(msgs)
	if len(got) != 3 {
		t.Fatalf("expected 3 unique messages, got %d", len(got))
	}
	if got[0].TgMessageID != "1" || got[1].TgMessageID != "2" || got[2].TgMessageID != "3" {
		t.Fatalf("unexpected order: %v", got)
	}
}

func TestBuildRAGPrompt(t *testing.T) {
	msgs := []ContextMessage{
		{Text: "деплой сломался", UserHash: "abc123", Timestamp: 1700000000},
	}
	prompt := buildRAGPrompt("Что с деплоем?", msgs)

	if !strings.Contains(prompt, "Что с деплоем?") {
		t.Fatal("prompt should contain the question")
	}
	if !strings.Contains(prompt, "деплой сломался") {
		t.Fatal("prompt should contain message text")
	}
	if !strings.Contains(prompt, "abc123") {
		t.Fatal("prompt should contain user hash")
	}
}

func TestFormatRAGResponseMarkdown(t *testing.T) {
	sources := []ContextMessage{
		{TgMessageID: "42", GroupID: "-1001234567890"},
		{TgMessageID: "43", GroupID: "-1001234567890"},
	}
	result := FormatRAGResponseMarkdown("Ответ тут.", sources, -1001234567890)

	if !strings.Contains(result, "Ответ тут\\.") {
		t.Fatal("response should contain escaped answer")
	}
	if !strings.Contains(result, "Источники") {
		t.Fatal("response should contain sources section")
	}
	if !strings.Contains(result, "t.me/c/") {
		t.Fatal("response should contain Telegram links")
	}
}

func TestTelegramMsgLink(t *testing.T) {
	tests := []struct {
		groupID int64
		msgID   string
		want    string
	}{
		{-1001234567890, "42", "https://t.me/c/1234567890/42"},
		{-1001234567890, "0", ""},
		{123, "42", ""},  // positive group ID
		{-123, "42", ""}, // not a supergroup
	}
	for _, tt := range tests {
		got := telegramMsgLink(tt.groupID, tt.msgID)
		if got != tt.want {
			t.Errorf("telegramMsgLink(%d, %q) = %q, want %q", tt.groupID, tt.msgID, got, tt.want)
		}
	}
}
