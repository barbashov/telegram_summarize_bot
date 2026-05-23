package summarizer

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"telegram_summarize_bot/db"
	"telegram_summarize_bot/logger"
	"telegram_summarize_bot/provider"
)

// negativeCacheTTL is how long a vision-call failure is remembered before we
// try again. Short so transient failures don't permanently darken an image,
// long enough that we don't hammer a misbehaving model.
const negativeCacheTTL = 24 * time.Hour

// maxDescriptionRunes caps the length of a stored description. Descriptions
// get pasted into a context-heavy summarizer prompt; oversized ones crowd out
// actual conversation. 600 runes ≈ a few short paragraphs.
const maxDescriptionRunes = 600

// visionMaxTokens is the per-image vision-call budget. 200 tokens is enough
// for a 60-word Russian description per the system prompt; 400 keeps headroom
// for screenshots heavy on transcribed text.
const visionMaxTokens = 400

const visionSystemPromptRU = `Опиши кратко (1–3 предложения) что изображено и какой текст виден.
Если это скриншот соцсети (Twitter/Reddit/HackerNews и т.п.) — приведи автора, тему и суть поста.
Только факты, без интерпретаций. Только русский. Максимум 60 слов.`

// ErrFileExpired mirrors handlers.ErrFileExpired so the summarizer package
// doesn't need to import handlers. PhotoFetcher implementations must return
// this exact value (or wrap it with errors.Is-compatible wrapping) when
// Telegram has rotated the file_id out from under us.
var ErrFileExpired = errors.New("telegram file expired")

// PhotoFetcher abstracts how the describer obtains image bytes for a given
// Telegram file_id. The real implementation lives on *handlers.Bot; tests
// substitute a stub.
type PhotoFetcher interface {
	FetchImage(ctx context.Context, fileID string) ([]byte, string, error)
}

// ImageDescriber returns a short textual description of a stored photo,
// using a cache to avoid repeated vision-model calls on the same image.
// Returning "" with nil error means "no description available, skip" —
// callers must treat that as a non-fatal degradation.
type ImageDescriber interface {
	Describe(ctx context.Context, photo db.PhotoRecord) (string, error)
}

// describerDB is the subset of *db.DB the cached describer needs. Defined
// here as an interface to keep tests light.
type describerDB interface {
	GetImageDescription(ctx context.Context, fileUniqueID string) (*db.ImageDescription, error)
	PutImageDescription(ctx context.Context, d db.ImageDescription) error
	TouchImageDescription(ctx context.Context, fileUniqueID string) error
}

// CachedDescriber is the production ImageDescriber. It looks up cached
// descriptions in DB, falls back to a vision-model call on miss, and stores
// the result (or a negative-cache entry on error).
type CachedDescriber struct {
	db          describerDB
	client      provider.LLMClient
	fetcher     PhotoFetcher
	model       string
	timeout     time.Duration
	systemPromp string
}

// NewCachedDescriber wires up a CachedDescriber. timeout caps a single vision
// call (cache lookup is not subject to it).
func NewCachedDescriber(database describerDB, client provider.LLMClient, fetcher PhotoFetcher, model string, timeout time.Duration) *CachedDescriber {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &CachedDescriber{
		db:          database,
		client:      client,
		fetcher:     fetcher,
		model:       model,
		timeout:     timeout,
		systemPromp: visionSystemPromptRU,
	}
}

// Describe implements ImageDescriber. Order:
//  1. Cache hit (positive): touch last_used_at, return cached text.
//  2. Cache hit (negative, fresh): return "" without retrying.
//  3. Cache miss / stale negative: fetch bytes, call vision, persist result.
//
// Errors from the fetcher and the vision call are converted to negative-cache
// entries (with the file-expired case skipping the negative cache so a fresh
// re-upload can recover). The caller never receives a hard error — vision is
// best-effort.
func (d *CachedDescriber) Describe(ctx context.Context, photo db.PhotoRecord) (string, error) {
	if photo.FileUniqueID == "" {
		return "", nil
	}

	cached, err := d.db.GetImageDescription(ctx, photo.FileUniqueID)
	if err != nil {
		logger.Warn().Err(err).Str("file_unique_id", photo.FileUniqueID).Msg("image cache lookup failed; proceeding without cache")
	} else if cached != nil {
		if cached.Error == "" {
			_ = d.db.TouchImageDescription(ctx, photo.FileUniqueID)
			return cached.Description, nil
		}
		if time.Since(cached.CreatedAt) < negativeCacheTTL {
			return "", nil
		}
	}

	bytes, mime, err := d.fetcher.FetchImage(ctx, photo.FileID)
	if err != nil {
		if errors.Is(err, ErrFileExpired) {
			// file_id rotated; cache nothing — a re-upload may recover.
			logger.Warn().Str("file_unique_id", photo.FileUniqueID).Msg("DIAG image file expired; skipping")
			return "", nil
		}
		logger.Warn().Err(err).Str("file_unique_id", photo.FileUniqueID).Str("file_id", photo.FileID).Msg("DIAG image fetch failed")
		d.storeNegative(ctx, photo.FileUniqueID, err.Error())
		return "", nil
	}
	logger.Info().Int("bytes", len(bytes)).Str("mime", mime).Str("file_unique_id", photo.FileUniqueID).Msg("DIAG image fetched, calling vision")

	callCtx, cancel := context.WithTimeout(ctx, d.timeout)
	defer cancel()

	resp, err := d.client.Complete(callCtx, provider.CompletionRequest{
		Model: d.model,
		Messages: []provider.Message{
			{Role: "system", Content: d.systemPromp},
			{
				Role:    "user",
				Content: "Опиши это изображение по правилам системного промпта.",
				Images:  []provider.ImageInput{{Bytes: bytes, MIMEType: mime}},
			},
		},
		MaxTokens:   visionMaxTokens,
		Temperature: 0.1,
	})
	if err != nil {
		logger.Warn().Err(err).Str("file_unique_id", photo.FileUniqueID).Msg("vision call failed")
		d.storeNegative(ctx, photo.FileUniqueID, err.Error())
		return "", nil
	}

	desc := truncateRunes(strings.TrimSpace(resp.Content), maxDescriptionRunes)
	if desc == "" {
		d.storeNegative(ctx, photo.FileUniqueID, "empty description")
		return "", nil
	}

	now := time.Now()
	if err := d.db.PutImageDescription(ctx, db.ImageDescription{
		FileUniqueID: photo.FileUniqueID,
		Description:  desc,
		Model:        d.model,
		CreatedAt:    now,
		LastUsedAt:   now,
	}); err != nil {
		logger.Warn().Err(err).Str("file_unique_id", photo.FileUniqueID).Msg("failed to persist description; returning anyway")
	}
	return desc, nil
}

func (d *CachedDescriber) storeNegative(ctx context.Context, fileUniqueID, errMsg string) {
	now := time.Now()
	if err := d.db.PutImageDescription(ctx, db.ImageDescription{
		FileUniqueID: fileUniqueID,
		Description:  "",
		Model:        d.model,
		CreatedAt:    now,
		LastUsedAt:   now,
		Error:        errMsg,
	}); err != nil {
		logger.Warn().Err(err).Str("file_unique_id", fileUniqueID).Msg("failed to write negative cache")
	}
}

func truncateRunes(s string, maxRunes int) string {
	if utf8.RuneCountInString(s) <= maxRunes {
		return s
	}
	runes := []rune(s)
	if len(runes) > maxRunes {
		runes = runes[:maxRunes]
	}
	return strings.TrimRight(string(runes), " \t\n") + "…"
}
