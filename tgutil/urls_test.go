package tgutil

import (
	"reflect"
	"testing"
	"unicode/utf16"

	"github.com/mymmrac/telego"
)

func TestExtractURLsUTF16Offsets(t *testing.T) {
	// An emoji before the URL: 😀 is one rune but two UTF-16 code units, so a
	// rune-based slice would drift. Telegram reports offsets in UTF-16 units.
	text := "😀 https://example.com/article"
	url := "https://example.com/article"
	offset := len(utf16.Encode([]rune("😀 ")))
	length := len(utf16.Encode([]rune(url)))
	ents := []telego.MessageEntity{{Type: "url", Offset: offset, Length: length}}

	got := ExtractURLs(text, ents, 0)
	if want := []string{url}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ExtractURLs = %v, want %v", got, want)
	}
}

func TestExtractURLsTextLinkCapAndDedup(t *testing.T) {
	ents := []telego.MessageEntity{
		{Type: "text_link", URL: "https://one.example"},
		{Type: "text_link", URL: "https://two.example"},
		{Type: "text_link", URL: "https://one.example"}, // duplicate, dropped
		{Type: "bold"}, // ignored
		{Type: "text_link", URL: "https://three.example"},
	}

	if got, want := ExtractURLs("text", ents, 2), []string{"https://one.example", "https://two.example"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("capped ExtractURLs = %v, want %v", got, want)
	}

	want := []string{"https://one.example", "https://two.example", "https://three.example"}
	if got := ExtractURLs("text", ents, 0); !reflect.DeepEqual(got, want) {
		t.Fatalf("uncapped ExtractURLs = %v, want %v", got, want)
	}
}

func TestExtractURLsNone(t *testing.T) {
	if got := ExtractURLs("no links here", nil, 1); len(got) != 0 {
		t.Fatalf("ExtractURLs = %v, want empty", got)
	}
}

func TestExtractURLsFromText(t *testing.T) {
	text := "смотри https://a.example/x и (https://b.example/y), и снова https://a.example/x. конец https://c.example!"
	got := ExtractURLsFromText(text, 0)
	want := []string{"https://a.example/x", "https://b.example/y", "https://c.example"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ExtractURLsFromText = %v, want %v", got, want)
	}

	// limit caps the result.
	if got := ExtractURLsFromText(text, 1); !reflect.DeepEqual(got, []string{"https://a.example/x"}) {
		t.Fatalf("capped = %v, want one URL", got)
	}

	// No bare URL → empty (a text_link href is never recoverable from text).
	if got := ExtractURLsFromText("просто текст без ссылок", 0); len(got) != 0 {
		t.Fatalf("got %v, want empty", got)
	}
}

func TestExtractURLsOutOfRangeOffsetSkipped(t *testing.T) {
	ents := []telego.MessageEntity{{Type: "url", Offset: 5, Length: 100}}
	if got := ExtractURLs("short", ents, 0); len(got) != 0 {
		t.Fatalf("ExtractURLs = %v, want empty for out-of-range offset", got)
	}
}
