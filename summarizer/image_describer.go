package summarizer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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

// visionSteerMaxTokens is the budget for a user-steered vision call. Larger than
// the default since the user may ask to transcribe or explain in more detail.
const visionSteerMaxTokens = 600

// visionSteeredSystemPromptRU is used when the user supplies a steering prompt;
// it drops the fixed 60-word cap so the model can answer the actual request.
const visionSteeredSystemPromptRU = `Ответь на запрос пользователя об изображении.
Опирайся только на то, что видно на изображении. Только факты, без домыслов. Только русский язык.`

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
	// Describe returns a description of the photo. A non-empty steering prompt
	// asks the vision model to answer that specific request about the image.
	Describe(ctx context.Context, photo db.PhotoRecord, steering string) (string, error)
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
	// steerEnabled gates user-steered vision calls; when off, a steering prompt
	// is ignored for the image and the standard cached description is returned.
	steerEnabled bool
}

// NewCachedDescriber wires up a CachedDescriber. timeout caps a single vision
// call (cache lookup is not subject to it). steerEnabled allows user-steered
// vision calls (see Describe).
func NewCachedDescriber(database describerDB, client provider.LLMClient, fetcher PhotoFetcher, model string, timeout time.Duration, steerEnabled bool) *CachedDescriber {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &CachedDescriber{
		db:           database,
		client:       client,
		fetcher:      fetcher,
		model:        model,
		timeout:      timeout,
		systemPromp:  visionSystemPromptRU,
		steerEnabled: steerEnabled,
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
func (d *CachedDescriber) Describe(ctx context.Context, photo db.PhotoRecord, steering string) (string, error) {
	if photo.FileUniqueID == "" {
		return "", nil
	}

	// A user-steered call (when enabled) asks the vision model the user's
	// question and is cached under a composite key so repeats of the same
	// (image, prompt) are free. When steering is off/empty, behaviour is
	// identical to before: standard prompt, cache keyed by file_unique_id.
	steering = strings.TrimSpace(steering)
	steered := steering != "" && d.steerEnabled
	cacheKey := photo.FileUniqueID
	if steered {
		cacheKey = photo.FileUniqueID + "#" + steeringHash(steering)
	}

	cached, err := d.db.GetImageDescription(ctx, cacheKey)
	if err != nil {
		logger.Warn().Err(err).Str("cache_key", cacheKey).Msg("image cache lookup failed; proceeding without cache")
	} else if cached != nil {
		if cached.Error == "" {
			_ = d.db.TouchImageDescription(ctx, cacheKey)
			return cached.Description, nil
		}
		if time.Since(cached.CreatedAt) < negativeCacheTTL {
			return "", nil
		}
	}

	bytes, mime, err := d.fetcher.FetchImage(ctx, photo.FileID)
	if err != nil {
		if errors.Is(err, ErrFileExpired) {
			logger.Debug().Str("file_unique_id", photo.FileUniqueID).Msg("image file expired; skipping")
			return "", nil
		}
		logger.Warn().Err(err).Str("file_unique_id", photo.FileUniqueID).Msg("image fetch failed; negative-caching")
		d.storeNegative(ctx, cacheKey, err.Error())
		return "", nil
	}

	sysPrompt := d.systemPromp
	userPrompt := "Опиши это изображение по правилам системного промпта."
	maxTokens := visionMaxTokens
	if steered {
		sysPrompt = visionSteeredSystemPromptRU
		userPrompt = "Запрос пользователя: " + steering
		maxTokens = visionSteerMaxTokens
	}

	callCtx, cancel := context.WithTimeout(ctx, d.timeout)
	defer cancel()

	resp, err := d.client.Complete(callCtx, provider.CompletionRequest{
		Model: d.model,
		Messages: []provider.Message{
			{Role: "system", Content: sysPrompt},
			{
				Role:    "user",
				Content: userPrompt,
				Images:  []provider.ImageInput{{Bytes: bytes, MIMEType: mime}},
			},
		},
		MaxTokens:   maxTokens,
		Temperature: 0.1,
	})
	if err != nil {
		logger.Warn().Err(err).Str("cache_key", cacheKey).Msg("vision call failed")
		d.storeNegative(ctx, cacheKey, err.Error())
		return "", nil
	}

	desc := truncateRunes(strings.TrimSpace(resp.Content), maxDescriptionRunes)
	if desc == "" {
		d.storeNegative(ctx, cacheKey, "empty description")
		return "", nil
	}

	now := time.Now()
	if err := d.db.PutImageDescription(ctx, db.ImageDescription{
		FileUniqueID: cacheKey,
		Description:  desc,
		Model:        d.model,
		CreatedAt:    now,
		LastUsedAt:   now,
	}); err != nil {
		logger.Warn().Err(err).Str("cache_key", cacheKey).Msg("failed to persist description; returning anyway")
	}
	return desc, nil
}

// steeringHash returns a short stable hash of a normalized steering prompt, used
// to build a per-(image, prompt) cache key.
func steeringHash(steering string) string {
	norm := strings.Join(strings.Fields(strings.ToLower(steering)), " ")
	sum := sha256.Sum256([]byte(norm))
	return hex.EncodeToString(sum[:])[:8]
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
