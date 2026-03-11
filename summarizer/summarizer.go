package summarizer

import (
	"context"
	"fmt"
	"time"

	"github.com/sashabaranov/go-openai"
	"telegram_summarize_bot/logger"
)

type Summarizer struct {
	client *openai.Client
	model  string
}

func New(apiKey, baseURL, model string) (*Summarizer, error) {
	config := openai.DefaultConfig(apiKey)
	config.BaseURL = baseURL
	config.HTTPClient.Timeout = 120 * time.Second

	client := openai.NewClientWithConfig(config)

	return &Summarizer{
		client: client,
		model:  model,
	}, nil
}

func (s *Summarizer) Summarize(ctx context.Context, messagesText string) (string, error) {
	if messagesText == "" {
		return "Нет сообщений для суммаризации за последние 24 часа.", nil
	}

	prompt := fmt.Sprintf(`Ты - ассистент, который суммаризует групповые чаты в Telegram. 
Твоя задача - кратко и информативно суммаризовать сообщения из группового чата на русском языке.
Суммаризация должна быть:
- Краткой (2-5 предложений)
- Информативной (основные темы и важные моменты)
- На русском языке

Вот сообщения из чата:
---
%s
---

Напиши краткую суммаризацию на русском языке:`, messagesText)

	resp, err := s.client.CreateChatCompletion(ctx, openai.ChatCompletionRequest{
		Model: s.model,
		Messages: []openai.ChatCompletionMessage{
			{
				Role:    openai.ChatMessageRoleSystem,
				Content: "Ты - ассистент для суммаризации групповых чатов. Пиши кратко и только на русском языке.",
			},
			{
				Role:    openai.ChatMessageRoleUser,
				Content: prompt,
			},
		},
		MaxTokens:   500,
		Temperature: 0.3,
	})
	if err != nil {
		logger.Error().Err(err).Msg("failed to create chat completion")
		return "", fmt.Errorf("failed to create summary: %w", err)
	}

	if len(resp.Choices) == 0 {
		return "", fmt.Errorf("no choices returned from OpenRouter")
	}

	return resp.Choices[0].Message.Content, nil
}
