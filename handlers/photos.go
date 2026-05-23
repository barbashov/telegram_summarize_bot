package handlers

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/mymmrac/telego"

	"telegram_summarize_bot/db"
	"telegram_summarize_bot/httputil"
	"telegram_summarize_bot/summarizer"
)

// telegramFileAPIBase is the base URL for downloading Telegram-hosted files.
// Variable (not const) so tests can override it.
var telegramFileAPIBase = "https://api.telegram.org/file/bot"

// extractPhotoRecords pulls photo metadata off an incoming Telegram message.
// For msg.Photo (compressed photo) the largest PhotoSize wins. msg.Document
// is included only when its MIME type is image/*. The returned records carry
// the file_unique_id (cache key) and file_id (download handle); they have no
// MessageID set — the caller fills that in after db.AddMessage.
func extractPhotoRecords(msg *telego.Message) []db.PhotoRecord {
	if msg == nil {
		return nil
	}
	var out []db.PhotoRecord

	if largest := largestPhotoSize(msg.Photo); largest != nil {
		out = append(out, db.PhotoRecord{
			FileUniqueID: largest.FileUniqueID,
			FileID:       largest.FileID,
			MIMEType:     "image/jpeg", // Telegram compresses photos to JPEG
			FileSize:     int64(largest.FileSize),
			Width:        largest.Width,
			Height:       largest.Height,
			Source:       db.PhotoSourcePhoto,
		})
	}

	if doc := msg.Document; doc != nil && strings.HasPrefix(strings.ToLower(doc.MimeType), "image/") {
		width, height := 0, 0
		if doc.Thumbnail != nil {
			width = doc.Thumbnail.Width
			height = doc.Thumbnail.Height
		}
		out = append(out, db.PhotoRecord{
			FileUniqueID: doc.FileUniqueID,
			FileID:       doc.FileID,
			MIMEType:     doc.MimeType,
			FileSize:     doc.FileSize,
			Width:        width,
			Height:       height,
			Source:       db.PhotoSourceDocument,
		})
	}

	return out
}

// largestPhotoSize picks the highest-resolution variant by area.
func largestPhotoSize(sizes []telego.PhotoSize) *telego.PhotoSize {
	if len(sizes) == 0 {
		return nil
	}
	bestIdx := 0
	bestArea := sizes[0].Width * sizes[0].Height
	for i := 1; i < len(sizes); i++ {
		area := sizes[i].Width * sizes[i].Height
		if area > bestArea {
			bestArea = area
			bestIdx = i
		}
	}
	return &sizes[bestIdx]
}

// hasImageMedia reports whether a message carries any image-shaped attachment
// we know how to ingest. Used by the update handler to allow caption-only or
// pure-image messages past the empty-text guard.
func hasImageMedia(msg *telego.Message) bool {
	if msg == nil {
		return false
	}
	if len(msg.Photo) > 0 {
		return true
	}
	if doc := msg.Document; doc != nil && strings.HasPrefix(strings.ToLower(doc.MimeType), "image/") {
		return true
	}
	return false
}

// FetchImage downloads the image identified by fileID and returns its raw bytes
// plus the inferred MIME type. It enforces maxBytes (truncation = error) and
// returns ErrFileExpired when Telegram says the handle is no longer valid.
func (b *Bot) FetchImage(ctx context.Context, fileID string) (data []byte, mime string, err error) {
	if fileID == "" {
		return nil, "", fmt.Errorf("empty file_id")
	}

	file, ferr := b.telegram.GetFile(ctx, &telego.GetFileParams{FileID: fileID})
	if ferr != nil {
		if isFileExpiredErr(ferr) {
			return nil, "", summarizer.ErrFileExpired
		}
		return nil, "", fmt.Errorf("getFile: %w", ferr)
	}
	if file == nil || file.FilePath == "" {
		return nil, "", fmt.Errorf("getFile returned empty file_path")
	}

	url := telegramFileAPIBase + b.cfg.BotToken + "/" + file.FilePath
	client := httputil.NewClient(60 * time.Second)
	req, rerr := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if rerr != nil {
		return nil, "", rerr
	}
	resp, derr := client.Do(req)
	if derr != nil {
		return nil, "", derr
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone {
		return nil, "", summarizer.ErrFileExpired
	}
	if resp.StatusCode >= 400 {
		return nil, "", fmt.Errorf("download file: HTTP %d", resp.StatusCode)
	}

	maxBytes := b.cfg.ImageMaxBytes
	if maxBytes <= 0 {
		maxBytes = 5_000_000
	}
	limited := io.LimitReader(resp.Body, int64(maxBytes)+1)
	data, rerr = io.ReadAll(limited)
	if rerr != nil {
		return nil, "", rerr
	}
	if len(data) > maxBytes {
		return nil, "", fmt.Errorf("image exceeds %d bytes", maxBytes)
	}

	// Telegram's file CDN often serves photos with Content-Type
	// "application/octet-stream" rather than an image/* MIME, so we cannot
	// trust the header alone. When it isn't image-shaped, fall back to
	// magic-byte detection on the payload before rejecting.
	mime = resp.Header.Get("Content-Type")
	if !strings.HasPrefix(strings.ToLower(mime), "image/") {
		mime = http.DetectContentType(data)
	}
	if !strings.HasPrefix(strings.ToLower(mime), "image/") {
		return nil, "", fmt.Errorf("unexpected content type: %s", mime)
	}

	return data, mime, nil
}

// isFileExpiredErr inspects a telego error string for the telltale "file is
// temporarily unavailable" / "wrong file_id" responses. The library does not
// expose typed errors for these, so string-matching is the practical option.
func isFileExpiredErr(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "wrong file_id") ||
		strings.Contains(s, "file is temporarily unavailable") ||
		strings.Contains(s, "file not found")
}
