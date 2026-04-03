package handlers

import (
	"context"
	"fmt"
	"strings"

	"github.com/mymmrac/telego"
)

func (b *Bot) handleCommand(ctx context.Context, update telego.Update, command string) {
	msg := update.Message
	if msg == nil {
		return
	}

	parts := strings.Fields(command)
	if len(parts) == 0 {
		b.handleHelp(update)
		return
	}

	cmd := parts[0]

	switch cmd {
	case "summarize", "sub", "s":
		b.handleSummarize(ctx, update, parts[1:])
	case "schedule":
		b.handleSchedule(ctx, update, parts[1:])
	case "help":
		b.handleHelp(update)
	default:
		b.handleHelp(update)
	}
}

func (b *Bot) extractCommandFromMention(text string, entities []telego.MessageEntity) (string, error) {
	mention := "@" + b.username

	if strings.HasPrefix(strings.ToLower(text), mention) {
		cmd := text[len(mention):]
		cmd = strings.TrimSpace(cmd)
		return cmd, nil
	}

	runes := []rune(text)
	for _, entity := range entities {
		entityType := entity.Type
		if entityType == "mention" || entityType == "text_mention" {
			start := entity.Offset
			end := start + entity.Length
			if end > len(runes) {
				continue
			}
			entityText := string(runes[start:end])
			if strings.ToLower(entityText) == mention {
				cmd := strings.TrimSpace(string(runes[end:]))
				return cmd, nil
			}
		}
	}

	return "", fmt.Errorf("no bot mention found")
}
