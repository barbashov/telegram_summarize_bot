package handlers

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf16"

	"github.com/mymmrac/telego"
)

// maxSteeringChars caps the free-text steering prompt accepted on a reply, to
// keep prompts (and cost) bounded.
const maxSteeringChars = 500

func (b *Bot) handleCommand(ctx context.Context, update telego.Update, command string) {
	msg := update.Message
	if msg == nil {
		return
	}

	parts := strings.Fields(command)
	cmd := ""
	if len(parts) > 0 {
		cmd = strings.ToLower(parts[0])
	}

	// help and schedule stay explicit commands, even when replying.
	switch cmd {
	case "help":
		b.handleHelp(ctx, update)
		return
	case "schedule":
		b.handleSchedule(ctx, update, parts[1:])
		return
	}

	isSummarizeKeyword := cmd == "summarize" || cmd == "sub" || cmd == "s"

	// A reply that mentions the bot always means "act on that message" — the
	// summarize keyword is optional. Any text beyond an optional leading
	// summarize keyword steers the result.
	if msg.ReplyToMessage != nil {
		steering := strings.TrimSpace(command)
		if isSummarizeKeyword {
			steering = strings.TrimSpace(strings.TrimPrefix(steering, parts[0]))
		}
		b.handleSummarizeReply(ctx, update, truncateSteering(steering))
		return
	}

	// Non-reply: summarize keyword runs the 24h summary; anything else is help.
	if isSummarizeKeyword {
		b.handleSummarize(ctx, update, parts[1:])
		return
	}
	b.handleHelp(ctx, update)
}

// truncateSteering bounds a steering prompt to maxSteeringChars runes.
func truncateSteering(s string) string {
	if r := []rune(s); len(r) > maxSteeringChars {
		return strings.TrimSpace(string(r[:maxSteeringChars]))
	}
	return s
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
