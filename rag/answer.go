package rag

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"telegram_summarize_bot/summarizer"
)

type ContextMessage struct {
	Text        string
	UserHash    string
	Timestamp   int64
	TgMessageID string
	GroupID     string
}

func buildRAGPrompt(question string, messages []ContextMessage) string {
	var sb strings.Builder
	sb.WriteString("Ниже — сообщения из истории группового чата Telegram.\n")
	sb.WriteString("Ответь на вопрос пользователя на основе этих сообщений.\n")
	sb.WriteString("Если в сообщениях нет информации для ответа, скажи об этом.\n")
	sb.WriteString("Отвечай на русском языке.\n\n")
	sb.WriteString("Сообщения:\n---\n")

	for _, m := range messages {
		ts := time.Unix(m.Timestamp, 0).UTC().Format("2006-01-02 15:04")
		author := m.UserHash
		if author == "" {
			author = "anon"
		}
		fmt.Fprintf(&sb, "[%s] %s: %s\n", ts, author, m.Text)
	}

	sb.WriteString("---\n\nВопрос: ")
	sb.WriteString(question)
	return sb.String()
}

func FormatRAGResponseMarkdown(answer string, sources []ContextMessage, groupID int64) string {
	var sb strings.Builder
	sb.WriteString("🔍 ")
	sb.WriteString(summarizer.EscapeMarkdown(answer))

	seen := make(map[string]bool)
	var links []string
	for _, m := range sources {
		if m.TgMessageID == "" || m.TgMessageID == "0" {
			continue
		}
		link := telegramMsgLink(groupID, m.TgMessageID)
		if link == "" || seen[link] {
			continue
		}
		seen[link] = true
		links = append(links, link)
		if len(links) >= 5 {
			break
		}
	}

	if len(links) > 0 {
		sb.WriteString("\n\n*Источники:*\n")
		for i, link := range links {
			fmt.Fprintf(&sb, "[%d](%s) ", i+1, link)
		}
	}

	return sb.String()
}

func telegramMsgLink(groupID int64, tgMessageID string) string {
	msgID, err := strconv.ParseInt(tgMessageID, 10, 64)
	if err != nil || msgID == 0 || groupID >= 0 {
		return ""
	}
	s := strconv.FormatInt(-groupID, 10)
	if len(s) <= 3 || s[:3] != "100" {
		return ""
	}
	return fmt.Sprintf("https://t.me/c/%s/%d", s[3:], msgID)
}

func deduplicateAndSort(messages []ContextMessage) []ContextMessage {
	seen := make(map[string]bool)
	var unique []ContextMessage
	for _, m := range messages {
		key := m.TgMessageID
		if key == "" || key == "0" {
			key = fmt.Sprintf("%d_%s", m.Timestamp, m.Text[:min(len(m.Text), 50)])
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		unique = append(unique, m)
	}
	sort.Slice(unique, func(i, j int) bool {
		return unique[i].Timestamp < unique[j].Timestamp
	})
	return unique
}
