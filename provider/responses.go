package provider

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
	"github.com/openai/openai-go/responses"
)

type responsesClient struct {
	client *openai.Client
	token  string // mutable for OAuth token injection
}

// NewResponsesClient creates an LLMClient using the OpenAI Responses API.
func NewResponsesClient(token, endpoint string) (LLMClient, error) {
	client := openai.NewClient(
		option.WithAPIKey(token),
		option.WithBaseURL(endpoint),
		option.WithHTTPClient(HTTPClient(120*time.Second)),
	)

	return &responsesClient{
		client: &client,
		token:  token,
	}, nil
}

func (c *responsesClient) Complete(ctx context.Context, req CompletionRequest) (CompletionResponse, error) {
	// Separate system message as instructions, rest as input
	var instructions string
	var inputItems []responses.ResponseInputItemUnionParam

	for _, m := range req.Messages {
		switch m.Role {
		case "system":
			instructions = m.Content
		case "user", "assistant":
			role := responses.EasyInputMessageRoleUser
			if m.Role == "assistant" {
				role = responses.EasyInputMessageRoleAssistant
			}
			inputItems = append(inputItems, responses.ResponseInputItemUnionParam{
				OfMessage: &responses.EasyInputMessageParam{
					Role: role,
					Content: responses.EasyInputMessageContentUnionParam{
						OfString: openai.String(m.Content),
					},
				},
			})
		}
	}

	params := responses.ResponseNewParams{
		Model:           req.Model,
		Instructions:    openai.String(instructions),
		MaxOutputTokens: openai.Int(int64(req.MaxTokens)),
		Temperature:     openai.Float(float64(req.Temperature)),
		Input: responses.ResponseNewParamsInputUnion{
			OfInputItemList: responses.ResponseInputParam(inputItems),
		},
	}

	// Use per-request token (may be updated by OAuth client)
	resp, err := c.client.Responses.New(ctx, params, option.WithAPIKey(c.token))
	if err != nil {
		return CompletionResponse{}, wrapResponsesError(err)
	}

	if resp.Status == "failed" {
		return CompletionResponse{}, &APIError{
			HTTPStatusCode: 500,
			Message:        fmt.Sprintf("%s: %s", resp.Error.Code, resp.Error.Message),
		}
	}

	text := resp.OutputText()
	finishReason := "stop"
	if resp.Status == "incomplete" {
		finishReason = "length"
	}

	return CompletionResponse{
		Content:      text,
		FinishReason: finishReason,
	}, nil
}

func wrapResponsesError(err error) error {
	var apiErr *openai.Error
	if errors.As(err, &apiErr) {
		return &APIError{
			HTTPStatusCode: apiErr.StatusCode,
			Message:        apiErr.Message,
		}
	}
	return err
}
