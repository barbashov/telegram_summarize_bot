package provider

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
	"github.com/openai/openai-go/responses"
	"github.com/openai/openai-go/shared"
	"telegram_summarize_bot/logger"
)

type responsesClient struct {
	client    *openai.Client
	token     string // mutable for OAuth token injection
	accountID string // ChatGPT-Account-ID header, set for OAuth mode
}

// NewResponsesClient creates an LLMClient using the OpenAI Responses API.
func NewResponsesClient(token, endpoint string) (LLMClient, error) {
	httpClient := DebugHTTPClient(120 * time.Second)
	client := openai.NewClient(
		option.WithAPIKey(token),
		option.WithBaseURL(endpoint),
		option.WithHTTPClient(httpClient),
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
		Model:        req.Model,
		Instructions: openai.String(instructions),
		Input: responses.ResponseNewParamsInputUnion{
			OfInputItemList: responses.ResponseInputParam(inputItems),
		},
		Text: responses.ResponseTextConfigParam{
			Format: responses.ResponseFormatTextConfigUnionParam{
				OfText: openai.Ptr(shared.NewResponseFormatTextParam()),
			},
		},
	}

	// Build per-request options: token + optional account ID header for OAuth mode.
	reqOpts := []option.RequestOption{option.WithAPIKey(c.token)}
	if c.accountID != "" {
		// ChatGPT backend requires store=false; rejects max_output_tokens and temperature.
		params.Store = openai.Bool(false)
		reqOpts = append(reqOpts, option.WithHeader(HeaderAccountID, c.accountID))
		reqOpts = append(reqOpts, option.WithHeader("version", CodexClientVersion))
		reqOpts = append(reqOpts, option.WithHeader("originator", CodexOriginator))
		return c.completeStreaming(ctx, params, reqOpts)
	}

	// Standard API supports max_output_tokens and temperature.
	params.MaxOutputTokens = openai.Int(int64(req.MaxTokens))
	params.Temperature = openai.Float(float64(req.Temperature))
	resp, err := c.client.Responses.New(ctx, params, reqOpts...)
	if err != nil {
		return CompletionResponse{}, wrapResponsesError(err)
	}

	return buildResponse(resp)
}

// completeStreaming uses the streaming API (required by ChatGPT backend) and
// collects the final response from the response.completed event.
func (c *responsesClient) completeStreaming(ctx context.Context, params responses.ResponseNewParams, reqOpts []option.RequestOption) (CompletionResponse, error) {
	stream := c.client.Responses.NewStreaming(ctx, params, reqOpts...)
	defer func() { _ = stream.Close() }()

	var finalResp *responses.Response
	var accumulated strings.Builder
	for stream.Next() {
		event := stream.Current()
		logger.Debug().Str("event_type", event.Type).Msg("streaming event")
		switch event.Type {
		case "response.output_text.delta":
			delta := event.AsResponseOutputTextDelta()
			accumulated.WriteString(delta.Delta)
		case "response.completed":
			completed := event.AsResponseCompleted()
			finalResp = &completed.Response
		}
	}
	if err := stream.Err(); err != nil {
		return CompletionResponse{}, wrapResponsesError(err)
	}
	if finalResp == nil {
		return CompletionResponse{}, &APIError{
			HTTPStatusCode: 500,
			Message:        "no response.completed event received",
		}
	}

	resp, err := buildResponse(finalResp)
	if err != nil {
		return CompletionResponse{}, err
	}

	if resp.Content == "" && accumulated.Len() > 0 {
		resp.Content = accumulated.String()
	}

	return resp, nil
}

func buildResponse(resp *responses.Response) (CompletionResponse, error) {
	if resp.Status == "failed" {
		return CompletionResponse{}, &APIError{
			HTTPStatusCode: 500,
			Message:        fmt.Sprintf("%s: %s", resp.Error.Code, resp.Error.Message),
		}
	}

	logger.Debug().RawJSON("raw_response", []byte(resp.RawJSON())).Msg("API response")

	text := resp.OutputText()
	finishReason := "stop"
	if resp.Status == "incomplete" {
		finishReason = "length"
	}

	return CompletionResponse{
		Content:        text,
		FinishReason:   finishReason,
		HTTPStatusCode: 200,
	}, nil
}

func wrapResponsesError(err error) error {
	var apiErr *openai.Error
	if errors.As(err, &apiErr) {
		msg := apiErr.Message
		if msg == "" {
			msg = apiErr.Error()
		}
		return &APIError{
			HTTPStatusCode: apiErr.StatusCode,
			Message:        msg,
		}
	}
	return err
}
