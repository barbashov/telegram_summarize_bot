package provider

import (
	"context"
	"errors"
	"time"

	"github.com/sashabaranov/go-openai"
)

type completionsClient struct {
	client *openai.Client
}

// NewCompletionsClient creates an LLMClient using the OpenAI Chat Completions API.
// Works with any OpenAI-compatible endpoint (OpenRouter, LiteLLM, etc.).
func NewCompletionsClient(token, endpoint string) (LLMClient, error) {
	cfg := openai.DefaultConfig(token)
	cfg.BaseURL = endpoint
	cfg.HTTPClient = HTTPClient(120 * time.Second)
	return &completionsClient{client: openai.NewClientWithConfig(cfg)}, nil
}

func (c *completionsClient) Complete(ctx context.Context, req CompletionRequest) (CompletionResponse, error) {
	messages := make([]openai.ChatCompletionMessage, len(req.Messages))
	for i, m := range req.Messages {
		messages[i] = openai.ChatCompletionMessage{
			Role:    m.Role,
			Content: m.Content,
		}
	}

	resp, err := c.client.CreateChatCompletion(ctx, openai.ChatCompletionRequest{
		Model:       req.Model,
		Messages:    messages,
		MaxTokens:   req.MaxTokens,
		Temperature: req.Temperature,
	})
	if err != nil {
		return CompletionResponse{}, wrapOpenAIError(err)
	}

	if len(resp.Choices) == 0 {
		return CompletionResponse{}, &APIError{
			HTTPStatusCode: 0,
			Message:        "no choices returned from API",
		}
	}

	return CompletionResponse{
		Content:        resp.Choices[0].Message.Content,
		FinishReason:   string(resp.Choices[0].FinishReason),
		HTTPStatusCode: 200,
	}, nil
}

func wrapOpenAIError(err error) error {
	var apiErr *openai.APIError
	if errors.As(err, &apiErr) {
		return &APIError{
			HTTPStatusCode: apiErr.HTTPStatusCode,
			Message:        apiErr.Message,
		}
	}
	return err
}
