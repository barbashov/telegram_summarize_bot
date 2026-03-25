package rag

import (
	"context"
	"fmt"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/sashabaranov/go-openai"
	"telegram_summarize_bot/config"
	"telegram_summarize_bot/logger"
)

const (
	workerChanCap   = 10000
	batchSize       = 50
	batchTimeout    = 5 * time.Second
	minMessageRunes = 10
	ragSystemPrompt = "Ты отвечаешь на вопросы по истории группового чата Telegram. Отвечай на русском языке."
	ragMaxTokens    = 1000
)

// UUID5 namespace for deterministic point IDs.
var pointIDNamespace = uuid.MustParse("a1b2c3d4-e5f6-7890-abcd-ef1234567890")

type chatClient interface {
	CreateChatCompletion(ctx context.Context, req openai.ChatCompletionRequest) (openai.ChatCompletionResponse, error)
}

// Message is the input type for Enqueue — mirrors the fields needed from db.Message.
type Message struct {
	GroupID       int64
	UserHash      string
	Text          string
	Timestamp     time.Time
	ForwardedFrom string
	TgMessageID   int64
	ReplyToTgID   int64
}

type Service struct {
	embedder *embedder
	store    *qdrantStore
	llm      chatClient
	model    string
	cfg      *config.Config
	ch       chan Message
}

func New(cfg *config.Config, llmClient chatClient, model string) (*Service, error) {
	emb := newEmbedder(cfg.EmbeddingAPIKey, cfg.EmbeddingURL, cfg.EmbeddingModel, cfg.EmbeddingDims)

	store, err := newQdrantStore(cfg.QdrantAddr, cfg.QdrantCollection)
	if err != nil {
		return nil, fmt.Errorf("rag: failed to connect to qdrant: %w", err)
	}

	return &Service{
		embedder: emb,
		store:    store,
		llm:      llmClient,
		model:    model,
		cfg:      cfg,
		ch:       make(chan Message, workerChanCap),
	}, nil
}

func (s *Service) Close() error {
	return s.store.close()
}

func (s *Service) Enqueue(msg Message) {
	if utf8.RuneCountInString(msg.Text) < minMessageRunes {
		return
	}
	select {
	case s.ch <- msg:
	default:
		logger.Warn().Msg("rag: enqueue channel full, dropping message")
	}
}

func (s *Service) StartWorker(ctx context.Context) {
	// Ensure collection exists. Determine dims from a probe embedding.
	dims := uint64(s.cfg.EmbeddingDims)
	if dims == 0 {
		vec, err := s.embedder.embedSingle(ctx, "probe")
		if err != nil {
			logger.Error().Err(err).Msg("rag: probe embedding failed, worker not started")
			return
		}
		dims = uint64(len(vec))
		logger.Info().Uint64("dims", dims).Msg("rag: auto-detected embedding dimensions")
	}

	if err := s.store.ensureCollection(ctx, dims); err != nil {
		logger.Error().Err(err).Msg("rag: failed to ensure qdrant collection, worker not started")
		return
	}

	go s.worker(ctx)
	logger.Info().Msg("rag: embedding worker started")
}

func (s *Service) worker(ctx context.Context) {
	var batch []Message
	timer := time.NewTimer(batchTimeout)
	defer timer.Stop()

	flush := func() {
		if len(batch) == 0 {
			return
		}
		if err := s.processBatch(ctx, batch); err != nil {
			logger.Error().Err(err).Int("count", len(batch)).Msg("rag: batch processing failed")
		}
		batch = batch[:0]
	}

	for {
		select {
		case <-ctx.Done():
			flush()
			return
		case msg, ok := <-s.ch:
			if !ok {
				flush()
				return
			}
			batch = append(batch, msg)
			if len(batch) >= batchSize {
				flush()
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(batchTimeout)
			}
		case <-timer.C:
			flush()
			timer.Reset(batchTimeout)
		}
	}
}

func (s *Service) processBatch(ctx context.Context, msgs []Message) error {
	texts := make([]string, len(msgs))
	for i, m := range msgs {
		texts[i] = m.Text
	}

	vectors, err := s.embedder.embed(ctx, texts)
	if err != nil {
		return fmt.Errorf("embedding batch: %w", err)
	}

	points := make([]vectorPoint, len(msgs))
	for i, m := range msgs {
		points[i] = vectorPoint{
			ID:            PointID(m.GroupID, m.TgMessageID),
			Vector:        vectors[i],
			GroupID:       strconv.FormatInt(m.GroupID, 10),
			Timestamp:     m.Timestamp.Unix(),
			TgMessageID:   strconv.FormatInt(m.TgMessageID, 10),
			Text:          m.Text,
			UserHash:      m.UserHash,
			ForwardedFrom: m.ForwardedFrom,
			ReplyToTgID:   strconv.FormatInt(m.ReplyToTgID, 10),
		}
	}

	return s.store.upsert(ctx, points)
}

// PointID returns a deterministic UUID5 for a group+message pair.
func PointID(groupID, tgMessageID int64) string {
	name := fmt.Sprintf("%d:%d", groupID, tgMessageID)
	return uuid.NewSHA1(pointIDNamespace, []byte(name)).String()
}

func (s *Service) Ask(ctx context.Context, groupID int64, question string) (string, []ContextMessage, error) {
	// 1. Embed the question.
	qVec, err := s.embedder.embedSingle(ctx, question)
	if err != nil {
		return "", nil, fmt.Errorf("rag ask: embed question: %w", err)
	}

	gidStr := strconv.FormatInt(groupID, 10)

	// 2. Semantic search.
	hits, err := s.store.search(ctx, qVec, gidStr, uint64(s.cfg.RAGTopK))
	if err != nil {
		return "", nil, fmt.Errorf("rag ask: search: %w", err)
	}
	if len(hits) == 0 {
		return "Не найдено релевантных сообщений в истории чата.", nil, nil
	}

	// 3. Context expansion: fetch messages within ±RAGContextWindow seconds of each hit.
	var allMessages []ContextMessage
	for _, hit := range hits {
		allMessages = append(allMessages, ContextMessage{
			Text:        hit.Text,
			UserHash:    hit.UserHash,
			Timestamp:   hit.Timestamp,
			TgMessageID: hit.TgMessageID,
			GroupID:     hit.GroupID,
		})

		windowSec := int64(s.cfg.RAGContextWindow)
		nearby, err := s.store.searchByTimeRange(ctx, gidStr, hit.Timestamp-windowSec, hit.Timestamp+windowSec)
		if err != nil {
			logger.Warn().Err(err).Msg("rag: context expansion scroll failed")
			continue
		}
		for _, n := range nearby {
			allMessages = append(allMessages, ContextMessage{
				Text:        n.Text,
				UserHash:    n.UserHash,
				Timestamp:   n.Timestamp,
				TgMessageID: n.TgMessageID,
				GroupID:     n.GroupID,
			})
		}
	}

	// 4. Dedup and sort chronologically.
	allMessages = deduplicateAndSort(allMessages)

	// 5. Build RAG prompt and call LLM.
	prompt := buildRAGPrompt(question, allMessages)

	resp, err := s.llm.CreateChatCompletion(ctx, openai.ChatCompletionRequest{
		Model: s.model,
		Messages: []openai.ChatCompletionMessage{
			{Role: openai.ChatMessageRoleSystem, Content: ragSystemPrompt},
			{Role: openai.ChatMessageRoleUser, Content: prompt},
		},
		MaxTokens:   ragMaxTokens,
		Temperature: 0.3,
	})
	if err != nil {
		return "", nil, fmt.Errorf("rag ask: llm call: %w", err)
	}
	if len(resp.Choices) == 0 {
		return "", nil, fmt.Errorf("rag ask: no choices in LLM response")
	}

	answer := resp.Choices[0].Message.Content

	// Return the original search hits as sources (not expanded context).
	sources := make([]ContextMessage, 0, len(hits))
	for _, hit := range hits {
		sources = append(sources, ContextMessage{
			Text:        hit.Text,
			UserHash:    hit.UserHash,
			Timestamp:   hit.Timestamp,
			TgMessageID: hit.TgMessageID,
			GroupID:     hit.GroupID,
		})
	}

	return answer, sources, nil
}
