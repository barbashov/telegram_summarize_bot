package telegram

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"summary_bot/service"
	"summary_bot/storage"
)

// Client is a minimal Telegram Bot API client used for sending messages.
type Client struct {
	botToken string
	baseURL  string
	log      *log.Logger
	client   *http.Client
}

// NewClient constructs a new Telegram client.
func NewClient(token, baseURL string, logger *log.Logger) *Client {
	if baseURL == "" {
		baseURL = "https://api.telegram.org"
	}
	return &Client{
		botToken: token,
		baseURL:  strings.TrimRight(baseURL, "/"),
		log:      logger,
		client: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

// sendMessageRequest is the payload for sendMessage.
type sendMessageRequest struct {
	ChatID           int64  `json:"chat_id"`
	Text             string `json:"text"`
	ReplyToMessageID int64  `json:"reply_to_message_id,omitempty"`
	ParseMode        string `json:"parse_mode,omitempty"`
}

func (c *Client) apiURL(method string) string {
	return fmt.Sprintf("%s/bot%s/%s", c.baseURL, c.botToken, method)
}

// SendMessage posts a text message to a chat.
func (c *Client) SendMessage(ctx context.Context, chatID int64, text string, replyTo int64) error {
	payload := sendMessageRequest{
		ChatID:           chatID,
		Text:             text,
		ReplyToMessageID: replyTo,
	}

	buf, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.apiURL("sendMessage"), strings.NewReader(string(buf)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("telegram sendMessage failed: %s: %s", resp.Status, string(body))
	}
	return nil
}

// WebhookHandler handles incoming Telegram webhook updates.
type WebhookHandler struct {
	client     *Client
	summarizer *service.Summarizer
	store      storage.Store
	wl         *service.Whitelist
	log        *log.Logger
}

// NewWebhookHandler constructs a new WebhookHandler.
func NewWebhookHandler(client *Client, summarizer *service.Summarizer, store storage.Store, wl *service.Whitelist, logger *log.Logger) http.Handler {
	return &WebhookHandler{
		client:     client,
		summarizer: summarizer,
		store:      store,
		wl:         wl,
		log:        logger,
	}
}

// telegramUpdate mirrors the subset of Telegram's Update object we care about.
type telegramUpdate struct {
	UpdateID    int64            `json:"update_id"`
	Message     *telegramMessage `json:"message,omitempty"`
	ChannelPost *telegramMessage `json:"channel_post,omitempty"`
}

type telegramMessage struct {
	MessageID int64         `json:"message_id"`
	Date      int64         `json:"date"`
	Chat      telegramChat  `json:"chat"`
	From      *telegramUser `json:"from,omitempty"`
	Text      string        `json:"text,omitempty"`
}

type telegramChat struct {
	ID       int64  `json:"id"`
	Type     string `json:"type"`
	Title    string `json:"title,omitempty"`
	Username string `json:"username,omitempty"`
}

type telegramUser struct {
	ID       int64  `json:"id"`
	Username string `json:"username,omitempty"`
}

func (h *WebhookHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	// Basic origin validation: Telegram sends a secret token in the URL when
	// you configure the webhook. In production you should configure a random
	// path component and ensure it matches here. This implementation assumes
	// the surrounding HTTP server routes only the correct path to this handler.

	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		h.log.Printf("read body error: %v", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	var upd telegramUpdate
	if err := json.Unmarshal(body, &upd); err != nil {
		h.log.Printf("invalid update json: %v", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	msg := upd.Message
	if msg == nil && upd.ChannelPost != nil {
		msg = upd.ChannelPost
	}
	if msg == nil {
		w.WriteHeader(http.StatusOK)
		return
	}

	ctx := r.Context()

	// Persist message if in a whitelisted channel.
	channelID := msg.Chat.ID
	if h.wl.IsAllowed(channelID) {
		username := sqlNullString(msg.From)
		m := storage.Message{
			ChannelID: channelID,
			MessageID: msg.MessageID,
			SenderID:  msg.FromID(),
			Username:  username,
			Text:      msg.Text,
			Timestamp: time.Unix(msg.Date, 0).UTC(),
		}
		if err := h.store.InsertMessage(ctx, m); err != nil {
			h.log.Printf("insert message error: %v", err)
		}
	}

	// Detect mention commands.
	if !strings.Contains(msg.Text, "@summary_bot") {
		w.WriteHeader(http.StatusOK)
		return
	}

	// Parse command after mention.
	rawRange := parseRangeFromText(msg.Text)

	if !h.wl.IsAllowed(channelID) {
		// Optionally send a denial message; here we log and stay silent.
		h.log.Printf("mention in non-whitelisted channel %d", channelID)
		w.WriteHeader(http.StatusOK)
		return
	}

	res, err := h.summarizer.SummarizeChannel(ctx, time.Now(), service.SummaryRequest{
		ChannelID: channelID,
		RawRange:  rawRange,
	})
	if err != nil {
		// If the error looks like a time range parse error, send a help message.
		if strings.Contains(err.Error(), "time range") || strings.Contains(err.Error(), "window") {
			_ = h.client.SendMessage(ctx, channelID, "Could not parse requested time range. Use e.g. 'last 6 hours' or '2024-01-01 to 2024-01-02'.", msg.MessageID)
		} else {
			h.log.Printf("summarize error: %v", err)
			_ = h.client.SendMessage(ctx, channelID, "Failed to generate summary. Please try again later.", msg.MessageID)
		}
		w.WriteHeader(http.StatusOK)
		return
	}

	if err := h.client.SendMessage(ctx, channelID, res, msg.MessageID); err != nil {
		h.log.Printf("send summary error: %v", err)
	}

	w.WriteHeader(http.StatusOK)
}

func sqlNullString(u *telegramUser) (ns sql.NullString) {
	if u == nil || u.Username == "" {
		return sql.NullString{Valid: false}
	}
	return sql.NullString{String: u.Username, Valid: true}
}

func (m *telegramMessage) FromID() int64 {
	if m.From == nil {
		return 0
	}
	return m.From.ID
}

// parseRangeFromText extracts the time range expression following the
// @summary_bot mention. Examples:
//
//	"@summary_bot" -> ""
//	"@summary_bot summarize last 3 hours" -> "last 3 hours"
//	"@summary_bot summarize 2024-01-01 to 2024-01-02" -> "2024-01-01 to 2024-01-02"
func parseRangeFromText(text string) string {
	lower := strings.ToLower(text)
	idx := strings.Index(lower, "@summary_bot")
	if idx == -1 {
		return ""
	}

	after := strings.TrimSpace(text[idx+len("@summary_bot"):])
	if after == "" {
		return ""
	}

	// Remove optional leading "summarize" keyword.
	afterLower := strings.ToLower(after)
	if strings.HasPrefix(afterLower, "summarize") {
		after = strings.TrimSpace(after[len("summarize"):])
	}
	return after
}
