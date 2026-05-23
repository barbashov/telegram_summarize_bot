package handlers

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"telegram_summarize_bot/db"
	"telegram_summarize_bot/fetcher"
	"telegram_summarize_bot/summarizer"

	"github.com/mymmrac/telego"
)

// seedChain inserts a root→leaf reply chain in group 42 and returns the leaf's
// TgMessageID. Each spec is {tgID, replyToTgID, text}; photos attach to the
// message whose tgID == photoOn (0 = none).
func seedChain(t *testing.T, database *db.DB, photoOn int64) {
	t.Helper()
	ctx := context.Background()
	specs := []struct {
		tg, replyTo int64
		text        string
	}{
		{10, 0, "первое сообщение про https://a.example/x"},
		{11, 10, "ответ с картинкой"},
		{12, 11, "последнее сообщение"},
	}
	for i, s := range specs {
		id, err := database.AddMessageReturningID(ctx, &db.Message{
			GroupID: 42, UserHash: "h" + string(rune('1'+i)), Text: s.text,
			Timestamp:   time.Now().Add(-time.Duration(len(specs)-i) * time.Minute),
			TgMessageID: s.tg, ReplyToTgID: s.replyTo,
		})
		if err != nil {
			t.Fatalf("seed AddMessageReturningID: %v", err)
		}
		if s.tg == photoOn {
			if err := database.AddMessagePhotos(ctx, id, []db.PhotoRecord{{FileUniqueID: "u1", FileID: "f1"}}); err != nil {
				t.Fatalf("seed AddMessagePhotos: %v", err)
			}
		}
	}
}

// threadReply builds the "@bot summarize" reply update whose target is the leaf
// (tg=12) of the seeded chain.
func threadReply() telego.Update {
	return replyUpdate(&telego.Message{MessageID: 12, Text: "последнее сообщение"})
}

// urlReply builds a reply message whose text contains url, with a matching
// "url" entity. The text is BMP-only, so rune offsets equal UTF-16 offsets.
func urlReply(prefix, url string) *telego.Message {
	return &telego.Message{
		MessageID: 100,
		Text:      prefix + url,
		Entities: []telego.MessageEntity{{
			Type:   "url",
			Offset: utf8.RuneCountInString(prefix),
			Length: utf8.RuneCountInString(url),
		}},
	}
}

// replyUpdate builds an "@bot summarize" update that is a reply to the given
// message in group 42.
func replyUpdate(reply *telego.Message) telego.Update {
	return telego.Update{
		Message: &telego.Message{
			Text:           "@testbot summarize",
			Chat:           telego.Chat{ID: 42, Type: "group"},
			From:           &telego.User{ID: 7, Username: "alice"},
			ReplyToMessage: reply,
		},
	}
}

func photoReply() *telego.Message {
	return &telego.Message{
		MessageID: 100,
		Photo:     []telego.PhotoSize{{FileID: "fid", FileUniqueID: "uid", Width: 100, Height: 100}},
	}
}

func TestHandleSummarizeReplyTextOnly(t *testing.T) {
	sum := &fakeSummarizer{textSummary: "Краткая выжимка темы"}
	b, database, tg := newTestBot(t, sum)
	defer func() { _ = database.Close() }()
	b.cfg.ReplyMinChars = 10

	reply := &telego.Message{MessageID: 100, Text: strings.Repeat("я", 50)}
	b.handleSummarizeReply(context.Background(), replyUpdate(reply), "")

	if sum.textCalls != 1 {
		t.Fatalf("SummarizeText calls = %d, want 1", sum.textCalls)
	}
	if sum.urlCalls != 0 || sum.imageCalls != 0 {
		t.Fatalf("unexpected url/image calls: %d/%d", sum.urlCalls, sum.imageCalls)
	}
	if len(tg.editTexts) != 1 || !strings.Contains(tg.editTexts[0], "Краткая выжимка темы") {
		t.Fatalf("unexpected result: %#v", tg.editTexts)
	}
}

func TestHandleSummarizeReplyTextTooShort(t *testing.T) {
	sum := &fakeSummarizer{}
	b, database, tg := newTestBot(t, sum)
	defer func() { _ = database.Close() }()
	b.cfg.ReplyMinChars = 100

	reply := &telego.Message{MessageID: 100, Text: "коротко"}
	b.handleSummarizeReply(context.Background(), replyUpdate(reply), "")

	if sum.textCalls != 0 {
		t.Fatalf("SummarizeText should not be called for short text, got %d", sum.textCalls)
	}
	if len(tg.sentTexts) != 1 || !strings.Contains(tg.sentTexts[0], "слишком короткое") {
		t.Fatalf("expected too-short message, got %#v", tg.sentTexts)
	}
	if len(tg.editTexts) != 0 {
		t.Fatalf("expected no edits, got %#v", tg.editTexts)
	}
}

func TestHandleSummarizeReplyImageOnly(t *testing.T) {
	sum := &fakeSummarizer{imageDesc: "На фото рыжий кот"}
	b, database, tg := newTestBot(t, sum)
	defer func() { _ = database.Close() }()

	b.handleSummarizeReply(context.Background(), replyUpdate(photoReply()), "")

	if sum.imageCalls != 1 {
		t.Fatalf("DescribeImage calls = %d, want 1", sum.imageCalls)
	}
	if sum.textCalls != 0 {
		t.Fatalf("a lone image should short-circuit, SummarizeText calls = %d", sum.textCalls)
	}
	if len(tg.editTexts) != 1 || !strings.Contains(tg.editTexts[0], "рыжий кот") {
		t.Fatalf("unexpected result: %#v", tg.editTexts)
	}
}

func TestHandleSummarizeReplyVisionDisabled(t *testing.T) {
	sum := &fakeSummarizer{imageErr: summarizer.ErrVisionDisabled}
	b, database, tg := newTestBot(t, sum)
	defer func() { _ = database.Close() }()

	b.handleSummarizeReply(context.Background(), replyUpdate(photoReply()), "")

	if len(tg.editTexts) != 1 || !strings.Contains(tg.editTexts[0], "Распознавание изображений отключено") {
		t.Fatalf("expected vision-disabled message, got %#v", tg.editTexts)
	}
}

func TestHandleSummarizeReplyUnsupportedMedia(t *testing.T) {
	sum := &fakeSummarizer{}
	b, database, tg := newTestBot(t, sum)
	defer func() { _ = database.Close() }()

	reply := &telego.Message{MessageID: 100, Voice: &telego.Voice{FileID: "v", FileUniqueID: "vu", Duration: 3}}
	b.handleSummarizeReply(context.Background(), replyUpdate(reply), "")

	if sum.textCalls != 0 || sum.imageCalls != 0 || sum.urlCalls != 0 {
		t.Fatalf("summarizer should not be called for unsupported media")
	}
	if len(tg.sentTexts) != 1 || !strings.Contains(tg.sentTexts[0], "не поддерживается") {
		t.Fatalf("expected unsupported message, got %#v", tg.sentTexts)
	}
}

func TestHandleSummarizeReplyMixedBlendsImageAndText(t *testing.T) {
	sum := &fakeSummarizer{imageDesc: "На фото кот", textSummary: "Единая выжимка"}
	b, database, tg := newTestBot(t, sum)
	defer func() { _ = database.Close() }()
	b.cfg.ReplyMinChars = 100 // caption is shorter than this on its own

	reply := photoReply()
	reply.Caption = "смешная подпись" // short, but folded in because an image is present
	b.handleSummarizeReply(context.Background(), replyUpdate(reply), "")

	if sum.imageCalls != 1 {
		t.Fatalf("DescribeImage calls = %d, want 1", sum.imageCalls)
	}
	if sum.textCalls != 1 {
		t.Fatalf("expected one blend SummarizeText call, got %d", sum.textCalls)
	}
	if !strings.Contains(sum.textInput, "На фото кот") {
		t.Fatalf("blend material missing image description: %q", sum.textInput)
	}
	if !strings.Contains(sum.textInput, "смешная подпись") {
		t.Fatalf("blend material missing message text: %q", sum.textInput)
	}
	if len(tg.editTexts) != 1 || !strings.Contains(tg.editTexts[0], "Единая выжимка") {
		t.Fatalf("unexpected result: %#v", tg.editTexts)
	}
}

func TestHandleSummarizeReplyAppliesGroupInstructions(t *testing.T) {
	sum := &fakeSummarizer{textSummary: "итог"}
	b, database, _ := newTestBot(t, sum)
	defer func() { _ = database.Close() }()
	b.cfg.ReplyMinChars = 10

	ctx := context.Background()
	if err := database.SetGroupSummaryInstructions(ctx, 42, 7, "выделяй риски"); err != nil {
		t.Fatalf("SetGroupSummaryInstructions error: %v", err)
	}

	reply := &telego.Message{MessageID: 100, Text: strings.Repeat("я", 50)}
	b.handleSummarizeReply(ctx, replyUpdate(reply), "")

	if sum.textInstr != "выделяй риски" {
		t.Fatalf("instructions passed to SummarizeText = %q, want %q", sum.textInstr, "выделяй риски")
	}
}

func TestHandleSummarizeReplyBareLink(t *testing.T) {
	sum := &fakeSummarizer{urlSummary: "Краткое содержание статьи"}
	b, database, tg := newTestBot(t, sum)
	defer func() { _ = database.Close() }()
	b.cfg.URLMaxChars = 64000

	ctx := context.Background()
	if err := database.SetGroupSummaryInstructions(ctx, 42, 7, "кратко"); err != nil {
		t.Fatalf("SetGroupSummaryInstructions error: %v", err)
	}

	var gotURL string
	var gotMax int
	b.fetchURL = func(_ context.Context, rawURL string, maxChars int) (string, error) {
		gotURL = rawURL
		gotMax = maxChars
		return "извлечённый текст страницы", nil
	}

	url := "https://example.com/article"
	// Bare URL with no surrounding prose → lone link, short-circuits.
	b.handleSummarizeReply(ctx, replyUpdate(urlReply("", url)), "")

	if gotURL != url {
		t.Fatalf("fetched URL = %q, want %q", gotURL, url)
	}
	if gotMax != 64000 {
		t.Fatalf("fetched maxChars = %d, want 64000", gotMax)
	}
	if sum.urlCalls != 1 {
		t.Fatalf("SummarizeURL calls = %d, want 1", sum.urlCalls)
	}
	if sum.textCalls != 0 {
		t.Fatalf("a lone link should short-circuit, SummarizeText calls = %d", sum.textCalls)
	}
	if sum.urlInstr != "кратко" {
		t.Fatalf("instructions passed to SummarizeURL = %q, want %q", sum.urlInstr, "кратко")
	}
	if len(tg.editTexts) != 1 || !strings.Contains(tg.editTexts[0], "Краткое содержание статьи") {
		t.Fatalf("unexpected result: %#v", tg.editTexts)
	}
}

func TestHandleSummarizeReplyLinkWithProseBlends(t *testing.T) {
	sum := &fakeSummarizer{urlSummary: "Содержание статьи", textSummary: "Единая выжимка"}
	b, database, tg := newTestBot(t, sum)
	defer func() { _ = database.Close() }()

	b.fetchURL = func(_ context.Context, _ string, _ int) (string, error) {
		return "извлечённый текст страницы", nil
	}

	// Surrounding prose is short but present → folded in alongside the link.
	b.handleSummarizeReply(context.Background(), replyUpdate(urlReply("важная мысль про ", "https://example.com/article")), "")

	if sum.urlCalls != 1 {
		t.Fatalf("SummarizeURL calls = %d, want 1", sum.urlCalls)
	}
	if sum.textCalls != 1 {
		t.Fatalf("expected a blend SummarizeText call, got %d", sum.textCalls)
	}
	if !strings.Contains(sum.textInput, "Содержание статьи") {
		t.Fatalf("blend material missing link summary: %q", sum.textInput)
	}
	if !strings.Contains(sum.textInput, "важная мысль про") {
		t.Fatalf("blend material missing prose: %q", sum.textInput)
	}
	if len(tg.editTexts) != 1 || !strings.Contains(tg.editTexts[0], "Единая выжимка") {
		t.Fatalf("unexpected result: %#v", tg.editTexts)
	}
}

func TestHandleSummarizeReplyLinkUnreadable(t *testing.T) {
	sum := &fakeSummarizer{}
	b, database, tg := newTestBot(t, sum)
	defer func() { _ = database.Close() }()

	b.fetchURL = func(_ context.Context, _ string, _ int) (string, error) {
		return "", fetcher.ErrNoReadableContent
	}

	b.handleSummarizeReply(context.Background(), replyUpdate(urlReply("", "https://dzen.ru/a/x")), "")

	if sum.urlCalls != 0 {
		t.Fatalf("SummarizeURL should not be called for an unreadable page, got %d", sum.urlCalls)
	}
	if len(tg.editTexts) != 1 || !strings.Contains(tg.editTexts[0], "требует входа") {
		t.Fatalf("expected unreadable-page message, got %#v", tg.editTexts)
	}
}

func TestHandleSummarizeReplyLinkFetchFails(t *testing.T) {
	sum := &fakeSummarizer{}
	b, database, tg := newTestBot(t, sum)
	defer func() { _ = database.Close() }()

	b.fetchURL = func(_ context.Context, _ string, _ int) (string, error) {
		return "", errors.New("boom")
	}

	// Bare URL so the only content is the (failing) link.
	b.handleSummarizeReply(context.Background(), replyUpdate(urlReply("", "https://example.com")), "")

	if sum.urlCalls != 0 {
		t.Fatalf("SummarizeURL should not be called when fetch fails, got %d", sum.urlCalls)
	}
	if len(tg.editTexts) != 1 || !strings.Contains(tg.editTexts[0], "Не удалось загрузить ссылку") {
		t.Fatalf("expected fetch-failure message, got %#v", tg.editTexts)
	}
}

func TestResidualText(t *testing.T) {
	// A "url" entity's span is the visible URL; stripping it leaves the prose.
	prefix := "важная мысль про "
	url := "https://example.com/article"
	ents := []telego.MessageEntity{{Type: "url", Offset: utf8.RuneCountInString(prefix), Length: utf8.RuneCountInString(url)}}
	if got := strings.TrimSpace(residualText(prefix+url, ents)); got != "важная мысль про" {
		t.Fatalf("residualText = %q, want %q", got, "важная мысль про")
	}

	// A bare URL leaves nothing.
	bareEnts := []telego.MessageEntity{{Type: "url", Offset: 0, Length: utf8.RuneCountInString(url)}}
	if got := strings.TrimSpace(residualText(url, bareEnts)); got != "" {
		t.Fatalf("residualText for bare URL = %q, want empty", got)
	}

	// No url entities → text unchanged.
	if got := residualText("просто текст", nil); got != "просто текст" {
		t.Fatalf("residualText = %q, want unchanged", got)
	}
}

func TestHandleSummarizeReplyShortTextWithSteering(t *testing.T) {
	sum := &fakeSummarizer{textSummary: "ответ"}
	b, database, _ := newTestBot(t, sum)
	defer func() { _ = database.Close() }()
	b.cfg.ReplyMinChars = 1000 // far above the message length

	reply := &telego.Message{MessageID: 100, Text: "кот"}
	b.handleSummarizeReply(context.Background(), replyUpdate(reply), "что это значит?")

	if sum.textCalls != 1 {
		t.Fatalf("a steering prompt should force summarization of short text; textCalls=%d", sum.textCalls)
	}
	if !strings.Contains(sum.textInstr, "что это значит?") {
		t.Fatalf("steering missing from instructions: %q", sum.textInstr)
	}
}

func TestHandleSummarizeReplySteeringCombinesWithGroupInstructions(t *testing.T) {
	sum := &fakeSummarizer{textSummary: "ответ"}
	b, database, _ := newTestBot(t, sum)
	defer func() { _ = database.Close() }()
	b.cfg.ReplyMinChars = 10

	ctx := context.Background()
	if err := database.SetGroupSummaryInstructions(ctx, 42, 7, "выделяй риски"); err != nil {
		t.Fatalf("SetGroupSummaryInstructions: %v", err)
	}

	reply := &telego.Message{MessageID: 100, Text: strings.Repeat("я", 50)}
	b.handleSummarizeReply(ctx, replyUpdate(reply), "как использовать?")

	if !strings.Contains(sum.textInstr, "выделяй риски") || !strings.Contains(sum.textInstr, "как использовать?") {
		t.Fatalf("combined instructions missing a part: %q", sum.textInstr)
	}
}

func TestHandleSummarizeReplyRendersMarkdown(t *testing.T) {
	// The LLM emits GitHub-style **bold**; the output must render it (no literal
	// ** markers leaking through, as in the reported screenshot).
	sum := &fakeSummarizer{textSummary: "Открыт раздел **Blues Factory** с отзывами"}
	b, database, tg := newTestBot(t, sum)
	defer func() { _ = database.Close() }()
	b.cfg.ReplyMinChars = 5

	reply := &telego.Message{MessageID: 100, Text: strings.Repeat("я", 50)}
	b.handleSummarizeReply(context.Background(), replyUpdate(reply), "")

	if len(tg.editTexts) != 1 {
		t.Fatalf("expected 1 edit, got %#v", tg.editTexts)
	}
	out := tg.editTexts[0]
	if strings.Contains(out, "**") {
		t.Fatalf("** markers should be converted to MarkdownV2, not left literal: %q", out)
	}
	if !strings.Contains(out, "Blues Factory") {
		t.Fatalf("content lost during conversion: %q", out)
	}
}

func TestHasUnsupportedMedia(t *testing.T) {
	tests := []struct {
		name string
		msg  *telego.Message
		want bool
	}{
		{"voice", &telego.Message{Voice: &telego.Voice{}}, true},
		{"video", &telego.Message{Video: &telego.Video{}}, true},
		{"sticker", &telego.Message{Sticker: &telego.Sticker{}}, true},
		{"animation", &telego.Message{Animation: &telego.Animation{}}, true},
		{"non-image document", &telego.Message{Document: &telego.Document{MimeType: "application/pdf"}}, true},
		{"image document", &telego.Message{Document: &telego.Document{MimeType: "image/png"}}, false},
		{"photo", &telego.Message{Photo: []telego.PhotoSize{{}}}, false},
		{"plain text", &telego.Message{Text: "hi"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := hasUnsupportedMedia(tt.msg); got != tt.want {
				t.Fatalf("hasUnsupportedMedia = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestHandleSummarizeReplyWalksChain(t *testing.T) {
	sum := &fakeSummarizer{urlSummary: "сводка ссылки", imageDesc: "описание картинки", textSummary: "итог обсуждения"}
	b, database, tg := newTestBot(t, sum)
	defer func() { _ = database.Close() }()
	b.cfg.ReplyThreads = true
	b.cfg.ReplyChainMaxLinks = 5
	b.cfg.ReplyChainMaxImages = 8

	var fetched int
	b.fetchURL = func(_ context.Context, _ string, _ int) (string, error) {
		fetched++
		return "извлечённый текст страницы", nil
	}
	seedChain(t, database, 11) // photo on the middle ancestor

	b.handleSummarizeReply(context.Background(), threadReply(), "")

	if sum.textCalls != 1 {
		t.Fatalf("expected one blend SummarizeText call, got %d", sum.textCalls)
	}
	if sum.urlCalls < 1 || fetched < 1 {
		t.Fatalf("expected the ancestor link to be fetched+summarized (url=%d fetch=%d)", sum.urlCalls, fetched)
	}
	if sum.imageCalls < 1 {
		t.Fatalf("expected the ancestor image to be described, got %d", sum.imageCalls)
	}
	mat := sum.textInput
	for _, want := range []string{"первое сообщение", "ответ с картинкой", "последнее сообщение", "сводка ссылки", "описание картинки"} {
		if !strings.Contains(mat, want) {
			t.Fatalf("thread material missing %q:\n%s", want, mat)
		}
	}
	if len(tg.editTexts) != 1 || !strings.Contains(tg.editTexts[0], "итог обсуждения") {
		t.Fatalf("unexpected result: %#v", tg.editTexts)
	}
}

func TestHandleSummarizeReplyChainLinkBudget(t *testing.T) {
	sum := &fakeSummarizer{urlSummary: "сводка", textSummary: "итог"}
	b, database, _ := newTestBot(t, sum)
	defer func() { _ = database.Close() }()
	b.cfg.ReplyThreads = true
	b.cfg.ReplyChainMaxLinks = 1 // only one link across the whole chain
	b.cfg.ReplyChainMaxImages = 8
	b.fetchURL = func(_ context.Context, _ string, _ int) (string, error) { return "page", nil }

	// Two ancestors each carry a link; target has none.
	ctx := context.Background()
	mustSeed := func(tg, replyTo int64, text string) {
		if _, err := database.AddMessageReturningID(ctx, &db.Message{
			GroupID: 42, UserHash: "h", Text: text, Timestamp: time.Now(), TgMessageID: tg, ReplyToTgID: replyTo,
		}); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	mustSeed(10, 0, "ссылка раз https://a.example/1")
	mustSeed(11, 10, "ссылка два https://b.example/2")
	mustSeed(12, 11, "последнее")

	b.handleSummarizeReply(ctx, threadReply(), "")

	if sum.urlCalls != 1 {
		t.Fatalf("link budget = 1 should cap SummarizeURL calls to 1, got %d", sum.urlCalls)
	}
}

func TestHandleSummarizeReplyThreadFallbackWhenTargetNotStored(t *testing.T) {
	// ReplyThreads on, but the target isn't in the DB → single-message path.
	sum := &fakeSummarizer{imageDesc: "кот"}
	b, database, tg := newTestBot(t, sum)
	defer func() { _ = database.Close() }()
	b.cfg.ReplyThreads = true

	b.handleSummarizeReply(context.Background(), replyUpdate(photoReply()), "")

	if sum.imageCalls != 1 || sum.textCalls != 0 {
		t.Fatalf("expected single-message image short-circuit (image=%d text=%d)", sum.imageCalls, sum.textCalls)
	}
	if len(tg.editTexts) != 1 || !strings.Contains(tg.editTexts[0], "кот") {
		t.Fatalf("unexpected result: %#v", tg.editTexts)
	}
}
