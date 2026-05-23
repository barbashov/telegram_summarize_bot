package provider

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/sashabaranov/go-openai"
)

type completionsClient struct {
	client *openai.Client
}

// NewCompletionsClient creates an LLMClient using the OpenAI Chat Completions API.
// Works with any OpenAI-compatible endpoint (OpenRouter, LiteLLM, etc.).
// A non-positive timeout falls back to defaultLLMHTTPTimeout.
func NewCompletionsClient(token, endpoint string, timeout time.Duration) (LLMClient, error) {
	if timeout <= 0 {
		timeout = defaultLLMHTTPTimeout
	}
	cfg := openai.DefaultConfig(token)
	cfg.BaseURL = endpoint
	cfg.HTTPClient = HTTPClient(timeout)
	return &completionsClient{client: openai.NewClientWithConfig(cfg)}, nil
}

func (c *completionsClient) Complete(ctx context.Context, req CompletionRequest) (CompletionResponse, error) {
	messages := make([]openai.ChatCompletionMessage, len(req.Messages))
	for i, m := range req.Messages {
		if len(m.Images) == 0 {
			messages[i] = openai.ChatCompletionMessage{
				Role:    m.Role,
				Content: m.Content,
			}
			continue
		}
		// Multimodal: send text + each image as a "image_url" part with a base64 data URI.
		parts := make([]openai.ChatMessagePart, 0, len(m.Images)+1)
		if m.Content != "" {
			parts = append(parts, openai.ChatMessagePart{
				Type: openai.ChatMessagePartTypeText,
				Text: m.Content,
			})
		}
		for _, img := range m.Images {
			parts = append(parts, openai.ChatMessagePart{
				Type: openai.ChatMessagePartTypeImageURL,
				ImageURL: &openai.ChatMessageImageURL{
					URL: dataURI(img),
				},
			})
		}
		messages[i] = openai.ChatCompletionMessage{
			Role:         m.Role,
			MultiContent: parts,
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

// dataURI builds an RFC-2397 data URL for an image payload. MIME defaults to
// image/jpeg when missing — the OpenAI/Chat Completions endpoints both accept
// JPEG/PNG/GIF/WebP without requiring an exact match.
func dataURI(img ImageInput) string {
	mime := img.MIMEType
	if mime == "" {
		mime = "image/jpeg"
	}
	return fmt.Sprintf("data:%s;base64,%s", mime, base64.StdEncoding.EncodeToString(img.Bytes))
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
