package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mymmrac/telego"

	"telegram_summarize_bot/config"
	"telegram_summarize_bot/db"
	"telegram_summarize_bot/summarizer"
)

func TestExtractPhotoRecords(t *testing.T) {
	t.Run("returns nil for empty message", func(t *testing.T) {
		if got := extractPhotoRecords(nil); got != nil {
			t.Fatalf("expected nil, got %+v", got)
		}
		if got := extractPhotoRecords(&telego.Message{}); got != nil {
			t.Fatalf("expected nil, got %+v", got)
		}
	})

	t.Run("picks largest photo by area", func(t *testing.T) {
		msg := &telego.Message{
			Photo: []telego.PhotoSize{
				{FileID: "small", FileUniqueID: "u-small", Width: 90, Height: 90},
				{FileID: "big", FileUniqueID: "u-big", Width: 1280, Height: 720},
				{FileID: "mid", FileUniqueID: "u-mid", Width: 320, Height: 240},
			},
		}
		got := extractPhotoRecords(msg)
		if len(got) != 1 {
			t.Fatalf("expected 1 record, got %d", len(got))
		}
		if got[0].FileUniqueID != "u-big" {
			t.Errorf("expected largest 'u-big', got %q", got[0].FileUniqueID)
		}
		if got[0].Source != db.PhotoSourcePhoto {
			t.Errorf("source = %q, want photo", got[0].Source)
		}
	})

	t.Run("includes image/* documents, excludes non-image", func(t *testing.T) {
		msg := &telego.Message{
			Document: &telego.Document{
				FileID:       "doc-id",
				FileUniqueID: "doc-uniq",
				MimeType:     "image/png",
				FileSize:     12345,
				Thumbnail: &telego.PhotoSize{
					Width: 320, Height: 240,
				},
			},
		}
		got := extractPhotoRecords(msg)
		if len(got) != 1 {
			t.Fatalf("expected 1 record, got %d", len(got))
		}
		if got[0].Source != db.PhotoSourceDocument {
			t.Errorf("source = %q, want document", got[0].Source)
		}
		if got[0].MIMEType != "image/png" {
			t.Errorf("mime = %q, want image/png", got[0].MIMEType)
		}
		if got[0].Width != 320 || got[0].Height != 240 {
			t.Errorf("expected dims from thumbnail, got %dx%d", got[0].Width, got[0].Height)
		}

		msg.Document.MimeType = "application/pdf"
		if got := extractPhotoRecords(msg); len(got) != 0 {
			t.Errorf("expected no records for non-image doc, got %d", len(got))
		}
	})

	t.Run("photo and image document both included", func(t *testing.T) {
		msg := &telego.Message{
			Photo: []telego.PhotoSize{
				{FileID: "p", FileUniqueID: "u-p", Width: 100, Height: 100},
			},
			Document: &telego.Document{
				FileID:       "d",
				FileUniqueID: "u-d",
				MimeType:     "image/jpeg",
			},
		}
		got := extractPhotoRecords(msg)
		if len(got) != 2 {
			t.Fatalf("expected 2 records, got %d", len(got))
		}
	})
}

func TestHasImageMedia(t *testing.T) {
	cases := []struct {
		name string
		msg  *telego.Message
		want bool
	}{
		{"nil", nil, false},
		{"text only", &telego.Message{Text: "hi"}, false},
		{"photo present", &telego.Message{Photo: []telego.PhotoSize{{FileID: "x"}}}, true},
		{"image document", &telego.Message{Document: &telego.Document{MimeType: "image/png"}}, true},
		{"non-image document", &telego.Message{Document: &telego.Document{MimeType: "application/pdf"}}, false},
		{"mixed-case mime", &telego.Message{Document: &telego.Document{MimeType: "Image/JPEG"}}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := hasImageMedia(c.msg); got != c.want {
				t.Errorf("hasImageMedia = %v, want %v", got, c.want)
			}
		})
	}
}

// fakeFileTelegram embeds fakeTelegram and overrides GetFile.
type fakeFileTelegram struct {
	fakeTelegram
	filePath string
	getErr   error
}

func (f *fakeFileTelegram) GetFile(_ context.Context, _ *telego.GetFileParams) (*telego.File, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	return &telego.File{FilePath: f.filePath}, nil
}

func TestFetchImageExpiredFileID(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer server.Close()

	prev := telegramFileAPIBase
	telegramFileAPIBase = server.URL + "/file/bot"
	defer func() { telegramFileAPIBase = prev }()

	b := &Bot{
		telegram: &fakeFileTelegram{filePath: "ok/path"},
		cfg:      &config.Config{BotToken: "tok", ImageMaxBytes: 1000},
	}
	_, _, err := b.FetchImage(context.Background(), "any-file")
	if err != summarizer.ErrFileExpired {
		t.Errorf("expected summarizer.ErrFileExpired, got %v", err)
	}
}

func TestFetchImageRejectsNonImageMime(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		_, _ = w.Write([]byte("%PDF"))
	}))
	defer server.Close()

	prev := telegramFileAPIBase
	telegramFileAPIBase = server.URL + "/file/bot"
	defer func() { telegramFileAPIBase = prev }()

	b := &Bot{
		telegram: &fakeFileTelegram{filePath: "ok"},
		cfg:      &config.Config{BotToken: "tok", ImageMaxBytes: 1000},
	}
	_, _, err := b.FetchImage(context.Background(), "any")
	if err == nil || !strings.Contains(err.Error(), "unexpected content type") {
		t.Errorf("expected MIME rejection, got %v", err)
	}
}

func TestFetchImageEnforcesByteLimit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write(make([]byte, 2000)) // exceeds limit
	}))
	defer server.Close()

	prev := telegramFileAPIBase
	telegramFileAPIBase = server.URL + "/file/bot"
	defer func() { telegramFileAPIBase = prev }()

	b := &Bot{
		telegram: &fakeFileTelegram{filePath: "ok"},
		cfg:      &config.Config{BotToken: "tok", ImageMaxBytes: 100},
	}
	_, _, err := b.FetchImage(context.Background(), "any")
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("expected size-limit error, got %v", err)
	}
}

func TestFetchImageHappyPath(t *testing.T) {
	body := []byte("\xff\xd8\xff\xe0fake-jpeg-bytes")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write(body)
	}))
	defer server.Close()

	prev := telegramFileAPIBase
	telegramFileAPIBase = server.URL + "/file/bot"
	defer func() { telegramFileAPIBase = prev }()

	b := &Bot{
		telegram: &fakeFileTelegram{filePath: "ok"},
		cfg:      &config.Config{BotToken: "tok", ImageMaxBytes: 10000},
	}
	data, mime, err := b.FetchImage(context.Background(), "any")
	if err != nil {
		t.Fatalf("FetchImage: %v", err)
	}
	if mime != "image/jpeg" {
		t.Errorf("mime = %q", mime)
	}
	if len(data) != len(body) {
		t.Errorf("data len = %d, want %d", len(data), len(body))
	}
}
