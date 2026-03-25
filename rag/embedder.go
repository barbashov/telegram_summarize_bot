package rag

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/sashabaranov/go-openai"
)

type embeddingClient interface {
	CreateEmbeddings(ctx context.Context, conv openai.EmbeddingRequestConverter) (openai.EmbeddingResponse, error)
}

type embedder struct {
	client embeddingClient
	model  openai.EmbeddingModel
	dims   int
}

func newEmbedder(apiKey, baseURL, model string, dims int) *embedder {
	cfg := openai.DefaultConfig(apiKey)
	cfg.BaseURL = baseURL
	cfg.HTTPClient = &http.Client{
		Timeout: 60 * time.Second,
		Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
		},
	}

	return &embedder{
		client: openai.NewClientWithConfig(cfg),
		model:  openai.EmbeddingModel(model),
		dims:   dims,
	}
}

func (e *embedder) embed(ctx context.Context, texts []string) ([][]float32, error) {
	req := openai.EmbeddingRequest{
		Input: texts,
		Model: e.model,
	}
	if e.dims > 0 {
		req.Dimensions = e.dims
	}

	resp, err := e.client.CreateEmbeddings(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("embedding API call failed: %w", err)
	}

	if len(resp.Data) != len(texts) {
		return nil, fmt.Errorf("expected %d embeddings, got %d", len(texts), len(resp.Data))
	}

	vectors := make([][]float32, len(resp.Data))
	for i, d := range resp.Data {
		vectors[i] = d.Embedding
	}
	return vectors, nil
}

func (e *embedder) embedSingle(ctx context.Context, text string) ([]float32, error) {
	vecs, err := e.embed(ctx, []string{text})
	if err != nil {
		return nil, err
	}
	return vecs[0], nil
}
