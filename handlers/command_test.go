package handlers

import (
	"context"
	"testing"

	"github.com/mymmrac/telego"
)

func TestHandleCommandReplyDispatch(t *testing.T) {
	t.Run("bare @bot reply runs the reply flow", func(t *testing.T) {
		sum := &fakeSummarizer{imageDesc: "кот"}
		b, database, _ := newTestBot(t, sum)
		defer func() { _ = database.Close() }()
		b.handleCommand(context.Background(), replyUpdate(photoReply()), "")
		if sum.imageCalls != 1 {
			t.Fatalf("bare @bot on a reply should run the reply flow; imageCalls=%d", sum.imageCalls)
		}
	})

	t.Run("free text steers and reaches the image", func(t *testing.T) {
		sum := &fakeSummarizer{imageDesc: "кот"}
		b, database, _ := newTestBot(t, sum)
		defer func() { _ = database.Close() }()
		b.handleCommand(context.Background(), replyUpdate(photoReply()), "опиши мем")
		if sum.imageSteering != "опиши мем" {
			t.Fatalf("steering passed to DescribeImage = %q, want %q", sum.imageSteering, "опиши мем")
		}
	})

	t.Run("leading summarize keyword is stripped from steering", func(t *testing.T) {
		sum := &fakeSummarizer{imageDesc: "кот"}
		b, database, _ := newTestBot(t, sum)
		defer func() { _ = database.Close() }()
		b.handleCommand(context.Background(), replyUpdate(photoReply()), "summarize опиши мем")
		if sum.imageSteering != "опиши мем" {
			t.Fatalf("steering after keyword = %q, want %q", sum.imageSteering, "опиши мем")
		}
	})

	t.Run("help stays a command even on a reply", func(t *testing.T) {
		sum := &fakeSummarizer{imageDesc: "кот"}
		b, database, _ := newTestBot(t, sum)
		defer func() { _ = database.Close() }()
		b.handleCommand(context.Background(), replyUpdate(photoReply()), "help")
		if sum.imageCalls != 0 {
			t.Fatalf("@bot help on a reply should show help, not run the reply flow; imageCalls=%d", sum.imageCalls)
		}
	})

	t.Run("non-reply free text shows help", func(t *testing.T) {
		sum := &fakeSummarizer{}
		b, database, _ := newTestBot(t, sum)
		defer func() { _ = database.Close() }()
		b.handleCommand(context.Background(), summarizeUpdate(), "describe the meme")
		if sum.calls != 0 || sum.textCalls != 0 || sum.imageCalls != 0 || sum.urlCalls != 0 {
			t.Fatalf("non-reply unknown command should not summarize anything")
		}
	})
}

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
		{
			name:    "prefix overlap with longer bot username",
			text:    "@testbotfriend summarize",
			wantErr: true,
		},
		{
			name:    "prefix overlap with trailing digit",
			text:    "@testbot2 summarize",
			wantErr: true,
		},
		{
			name: "mention entity after non-BMP rune",
			text: "🚀 @testbot summarize",
			entities: []telego.MessageEntity{
				// "🚀" is 2 UTF-16 units; space is 1; "@testbot" starts at offset 3.
				{Type: "mention", Offset: 3, Length: 8},
			},
			want: "summarize",
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
