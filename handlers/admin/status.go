package admin

import (
	"strings"

	"telegram_summarize_bot/logger"
	"telegram_summarize_bot/metrics"

	"github.com/mymmrac/telego"
	tu "github.com/mymmrac/telego/telegoutil"
)

func (a *Admin) handleStatus(chatID int64) {
	defer a.metrics.TelegramSend.Start()()
	keyboard := tu.InlineKeyboard(
		tu.InlineKeyboardRow(
			telego.InlineKeyboardButton{Text: "llm_cluster", CallbackData: "lat:llm_cluster"},
			telego.InlineKeyboardButton{Text: "llm_summarize", CallbackData: "lat:llm_summarize"},
		),
		tu.InlineKeyboardRow(
			telego.InlineKeyboardButton{Text: "telegram_send", CallbackData: "lat:telegram_send"},
			telego.InlineKeyboardButton{Text: "telegram_edit", CallbackData: "lat:telegram_edit"},
		),
		tu.InlineKeyboardRow(
			telego.InlineKeyboardButton{Text: "db_add", CallbackData: "lat:db_add"},
			telego.InlineKeyboardButton{Text: "db_get", CallbackData: "lat:db_get"},
		),
	)
	_, err := a.telegram.SendMessage(
		tu.Message(tu.ID(chatID), a.metrics.FormatStatusReport(a.cfg.Model)).
			WithReplyMarkup(keyboard),
	)
	if err != nil {
		logger.Error().Err(err).Int64("chat_id", chatID).Msg("failed to send status with keyboard")
		a.metrics.RecordError("telegram_send", err.Error())
	}
}

// HandleCallbackQuery processes inline button presses for metric deep-dives.
func (a *Admin) HandleCallbackQuery(cq *telego.CallbackQuery) {
	_ = a.telegram.AnswerCallbackQuery(&telego.AnswerCallbackQueryParams{
		CallbackQueryID: cq.ID,
	})

	if !a.cfg.IsAdminUser(cq.From.ID) {
		return
	}

	data := cq.Data
	if !strings.HasPrefix(data, "lat:") {
		return
	}
	metricName := strings.TrimPrefix(data, "lat:")

	validMetrics := map[string]bool{
		"llm_cluster": true, "llm_summarize": true,
		"telegram_send": true, "telegram_edit": true,
		"db_add": true, "db_get": true,
	}
	if !validMetrics[metricName] {
		return
	}

	detail := a.metrics.CachedLatency(metricName)
	text := metrics.FormatLatencyDeepDive(metricName, detail)

	if cq.Message != nil {
		a.deps.SendMessage(cq.Message.GetChat().ID, text)
	}
}
