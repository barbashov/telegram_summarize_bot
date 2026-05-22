package handlers

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf16"

	"github.com/mymmrac/telego"
)

func (b *Bot) handleCommand(ctx context.Context, update telego.Update, command string) {
	msg := update.Message
	if msg == nil {
		return
	}

	parts := strings.Fields(command)
	if len(parts) == 0 {
		b.handleHelp(ctx, update)
		return
	}

	cmd := parts[0]

	switch cmd {
	case "summarize", "sub", "s":
		b.handleSummarize(ctx, update, parts[1:])
	case "schedule":
		b.handleSchedule(ctx, update, parts[1:])
	case "help":
		b.handleHelp(ctx, update)
	default:
		b.handleHelp(ctx, update)
	}
}

func (b *Bot) extractCommandFromMention(text string, entities []telego.MessageEntity) (string, error) {
	mention := "@" + b.username

	if strings.HasPrefix(strings.ToLower(text), mention) {
		rest := text[len(mention):]
		if rest == "" || !isUsernameChar(rest[0]) {
			return strings.TrimSpace(rest), nil
		}
	}

	units := utf16.Encode([]rune(text))
	for _, entity := range entities {
		entityType := entity.Type
		if entityType != "mention" && entityType != "text_mention" {
			continue
		}
		start := entity.Offset
		end := start + entity.Length
		if start < 0 || end > len(units) {
			continue
		}
		entityText := string(utf16.Decode(units[start:end]))
		if strings.ToLower(entityText) == mention {
			return strings.TrimSpace(string(utf16.Decode(units[end:]))), nil
		}
	}

	return "", fmt.Errorf("no bot mention found")
}

// isUsernameChar reports whether b is a valid Telegram username character
// (ASCII letter, digit, or underscore). Used to detect a real word boundary
// after a bot-mention prefix.
func isUsernameChar(b byte) bool {
	switch {
	case b >= 'a' && b <= 'z':
		return true
	case b >= 'A' && b <= 'Z':
		return true
	case b >= '0' && b <= '9':
		return true
	case b == '_':
		return true
	}
	return false
}
