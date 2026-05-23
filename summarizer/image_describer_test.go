package summarizer

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"telegram_summarize_bot/db"
	"telegram_summarize_bot/provider"
)

// stubDB is the in-memory cache the describer tests assert against.
type stubDB struct {
	entries map[string]db.ImageDescription
	gets    int32
	puts    int32
	touches int32
	getErr  error
}

func newStubDB() *stubDB {
	return &stubDB{entries: map[string]db.ImageDescription{}}
}

func (s *stubDB) GetImageDescription(_ context.Context, fileUniqueID string) (*db.ImageDescription, error) {
	atomic.AddInt32(&s.gets, 1)
	if s.getErr != nil {
		return nil, s.getErr
	}
	d, ok := s.entries[fileUniqueID]
	if !ok {
		return nil, nil
	}
	return &d, nil
}

func (s *stubDB) PutImageDescription(_ context.Context, d db.ImageDescription) error {
	atomic.AddInt32(&s.puts, 1)
	s.entries[d.FileUniqueID] = d
	return nil
}

func (s *stubDB) TouchImageDescription(_ context.Context, fileUniqueID string) error {
	atomic.AddInt32(&s.touches, 1)
	if d, ok := s.entries[fileUniqueID]; ok {
		d.LastUsedAt = time.Now()
		s.entries[fileUniqueID] = d
	}
	return nil
}

// stubFetcher returns canned bytes (or an error sentinel) for a given file_id.
type stubFetcher struct {
	bytes []byte
	mime  string
	err   error
	calls int32
}

func (f *stubFetcher) FetchImage(_ context.Context, _ string) (data []byte, mime string, err error) {
	atomic.AddInt32(&f.calls, 1)
	return f.bytes, f.mime, f.err
}

// stubLLM lets us assert what the describer sent and control the response.
type stubLLM struct {
	resp  provider.CompletionResponse
	err   error
	calls int32
	req   provider.CompletionRequest
}

func (s *stubLLM) Complete(_ context.Context, req provider.CompletionRequest) (provider.CompletionResponse, error) {
	atomic.AddInt32(&s.calls, 1)
	s.req = req
	return s.resp, s.err
}

func TestCachedDescriber_CacheHitSkipsFetch(t *testing.T) {
	s := newStubDB()
	s.entries["k"] = db.ImageDescription{
		FileUniqueID: "k",
		Description:  "cached desc",
		Model:        "gpt-5.5",
		CreatedAt:    time.Now(),
		LastUsedAt:   time.Now(),
	}
	fetcher := &stubFetcher{}
	llm := &stubLLM{}

	d := NewCachedDescriber(s, llm, fetcher, "gpt-5.5", time.Second)
	got, err := d.Describe(context.Background(), db.PhotoRecord{FileUniqueID: "k", FileID: "fid"})
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if got != "cached desc" {
		t.Errorf("got %q, want cached desc", got)
	}
	if atomic.LoadInt32(&fetcher.calls) != 0 {
		t.Errorf("expected no fetch on cache hit, got %d", fetcher.calls)
	}
	if atomic.LoadInt32(&llm.calls) != 0 {
		t.Errorf("expected no LLM call on cache hit, got %d", llm.calls)
	}
	if atomic.LoadInt32(&s.touches) != 1 {
		t.Errorf("expected 1 Touch, got %d", s.touches)
	}
}

func TestCachedDescriber_FreshNegativeCacheSkipsCall(t *testing.T) {
	s := newStubDB()
	s.entries["k"] = db.ImageDescription{
		FileUniqueID: "k",
		Error:        "transient failure",
		CreatedAt:    time.Now().Add(-time.Hour), // fresh (<24h)
	}
	fetcher := &stubFetcher{bytes: []byte("x"), mime: "image/jpeg"}
	llm := &stubLLM{}

	d := NewCachedDescriber(s, llm, fetcher, "gpt-5.5", time.Second)
	got, err := d.Describe(context.Background(), db.PhotoRecord{FileUniqueID: "k", FileID: "fid"})
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if got != "" {
		t.Errorf("expected empty (negative cache), got %q", got)
	}
	if atomic.LoadInt32(&fetcher.calls) != 0 {
		t.Errorf("expected no fetch (fresh negative), got %d", fetcher.calls)
	}
	if atomic.LoadInt32(&llm.calls) != 0 {
		t.Errorf("expected no LLM call (fresh negative), got %d", llm.calls)
	}
}

func TestCachedDescriber_StaleNegativeCacheRetries(t *testing.T) {
	s := newStubDB()
	s.entries["k"] = db.ImageDescription{
		FileUniqueID: "k",
		Error:        "old failure",
		CreatedAt:    time.Now().Add(-48 * time.Hour), // stale (>24h)
	}
	fetcher := &stubFetcher{bytes: []byte("img"), mime: "image/jpeg"}
	llm := &stubLLM{resp: provider.CompletionResponse{Content: "fresh desc"}}

	d := NewCachedDescriber(s, llm, fetcher, "gpt-5.5", time.Second)
	got, err := d.Describe(context.Background(), db.PhotoRecord{FileUniqueID: "k", FileID: "fid"})
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if got != "fresh desc" {
		t.Errorf("expected fresh desc, got %q", got)
	}
}

func TestCachedDescriber_FileExpiredSkipsNegativeCache(t *testing.T) {
	s := newStubDB()
	fetcher := &stubFetcher{err: ErrFileExpired}
	llm := &stubLLM{}

	d := NewCachedDescriber(s, llm, fetcher, "gpt-5.5", time.Second)
	got, err := d.Describe(context.Background(), db.PhotoRecord{FileUniqueID: "k", FileID: "fid"})
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if got != "" {
		t.Errorf("expected empty, got %q", got)
	}
	if atomic.LoadInt32(&s.puts) != 0 {
		t.Errorf("expected no cache write on file-expired, got %d", s.puts)
	}
	if atomic.LoadInt32(&llm.calls) != 0 {
		t.Errorf("expected no LLM call when fetch failed, got %d", llm.calls)
	}
}

func TestCachedDescriber_VisionErrorWritesNegativeCache(t *testing.T) {
	s := newStubDB()
	fetcher := &stubFetcher{bytes: []byte("img"), mime: "image/jpeg"}
	llm := &stubLLM{err: errors.New("boom")}

	d := NewCachedDescriber(s, llm, fetcher, "gpt-5.5", time.Second)
	got, err := d.Describe(context.Background(), db.PhotoRecord{FileUniqueID: "k", FileID: "fid"})
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if got != "" {
		t.Errorf("expected empty on vision error, got %q", got)
	}
	entry, ok := s.entries["k"]
	if !ok {
		t.Fatal("expected negative cache entry to be written")
	}
	if entry.Error == "" {
		t.Errorf("expected non-empty error field, got %+v", entry)
	}
}

func TestCachedDescriber_HappyPathPersists(t *testing.T) {
	s := newStubDB()
	fetcher := &stubFetcher{bytes: []byte("img-bytes"), mime: "image/png"}
	llm := &stubLLM{resp: provider.CompletionResponse{Content: "  cat on a sill  "}}

	d := NewCachedDescriber(s, llm, fetcher, "gpt-5.5", time.Second)
	got, err := d.Describe(context.Background(), db.PhotoRecord{FileUniqueID: "k", FileID: "fid"})
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if got != "cat on a sill" {
		t.Errorf("expected trimmed text, got %q", got)
	}

	// Verify the request carried the image bytes.
	if len(llm.req.Messages) < 2 {
		t.Fatalf("expected ≥2 messages, got %d", len(llm.req.Messages))
	}
	userMsg := llm.req.Messages[1]
	if len(userMsg.Images) != 1 {
		t.Fatalf("expected 1 image, got %d", len(userMsg.Images))
	}
	if string(userMsg.Images[0].Bytes) != "img-bytes" {
		t.Errorf("image bytes not propagated: %q", userMsg.Images[0].Bytes)
	}
	if userMsg.Images[0].MIMEType != "image/png" {
		t.Errorf("mime not propagated: %q", userMsg.Images[0].MIMEType)
	}

	entry, ok := s.entries["k"]
	if !ok {
		t.Fatal("expected positive cache entry")
	}
	if entry.Description != "cat on a sill" {
		t.Errorf("persisted desc = %q", entry.Description)
	}
}

func TestTruncateRunes(t *testing.T) {
	if got := truncateRunes("abc", 10); got != "abc" {
		t.Errorf("short string mutated: %q", got)
	}
	long := "абвгдежзий"
	if got := truncateRunes(long, 3); got != "абв…" {
		t.Errorf("got %q, want 'абв…'", got)
	}
}
