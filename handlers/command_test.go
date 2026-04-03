package handlers

import (
	"testing"

	"github.com/mymmrac/telego"
)

func TestExtractCommandFromMention(t *testing.T) {
	b, database, _ := newTestBot(t, &fakeSummarizer{})
	defer func() { _ = database.Close() }()

	tests := []struct {
		name     string
		text     string
		entities []telego.MessageEntity
		want     string
		wantErr  bool
	}{
		{
			name: "prefix mention",
			text: "@testbot summarize",
			want: "summarize",
		},
		{
			name: "case insensitive prefix",
			text: "@TestBot help",
			want: "help",
		},
		{
			name:    "no mention",
			text:    "hello world",
			wantErr: true,
		},
		{
			name: "mention entity in middle",
			text: "hey @testbot summarize 12",
			entities: []telego.MessageEntity{
				{Type: "mention", Offset: 4, Length: 8},
			},
			want: "summarize 12",
		},
		{
			name: "empty command after mention",
			text: "@testbot",
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := b.extractCommandFromMention(tt.text, tt.entities)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}
