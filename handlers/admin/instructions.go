package admin

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"telegram_summarize_bot/db"
	"telegram_summarize_bot/logger"
	"telegram_summarize_bot/summarizer"

	"github.com/mymmrac/telego"
	tu "github.com/mymmrac/telego/telegoutil"
)

const (
	instructionsCallbackPrefix = "inst:"
)

func (a *Admin) handleInstructions(ctx context.Context, chatID int64) {
	groups, err := a.db.GetKnownGroups(ctx)
	if err != nil {
		logger.Error().Err(err).Msg("failed to get known groups for instructions")
		a.deps.SendMessage(ctx, chatID, "Ошибка получения списка групп.")
		return
	}

	var rows []telego.InlineKeyboardButton
	for _, g := range groups {
		if !g.Allowed {
			continue
		}
		title := strings.TrimSpace(g.Title)
		if title == "" {
			title = fmt.Sprintf("%d", g.GroupID)
		}
		rows = append(rows, telego.InlineKeyboardButton{
			Text:         title,
			CallbackData: fmt.Sprintf("inst:grp:%d", g.GroupID),
		})
	}
	if len(rows) == 0 {
		a.deps.SendMessage(ctx, chatID, "Нет разрешённых групп.")
		return
	}

	keyboardRows := make([][]telego.InlineKeyboardButton, 0, len(rows))
	for _, row := range rows {
		keyboardRows = append(keyboardRows, []telego.InlineKeyboardButton{row})
	}
	a.sendInstructionsKeyboard(ctx, chatID, "Выберите группу для настройки инструкций суммаризации:", keyboardRows)
}

func (a *Admin) handleInstructionsCallback(ctx context.Context, cq *telego.CallbackQuery) {
	chatID := callbackChatID(cq)
	if chatID == 0 {
		return
	}

	data := strings.TrimPrefix(cq.Data, instructionsCallbackPrefix)
	switch {
	case data == "cancel":
		a.clearPendingInstructions(chatID)
		a.deps.SendMessage(ctx, chatID, "Настройка инструкций отменена.")
	case strings.HasPrefix(data, "grp:"):
		groupID, ok := parseInstructionGroupID(strings.TrimPrefix(data, "grp:"))
		if !ok {
			return
		}
		if !a.ensureInstructionsGroupAllowed(ctx, chatID, groupID) {
			return
		}
		a.showInstructionsGroup(ctx, chatID, groupID)
	case strings.HasPrefix(data, "edit:"):
		groupID, ok := parseInstructionGroupID(strings.TrimPrefix(data, "edit:"))
		if !ok {
			return
		}
		if !a.ensureInstructionsGroupAllowed(ctx, chatID, groupID) {
			return
		}
		promptID := a.deps.SendMessage(ctx, chatID, fmt.Sprintf(
			"Ответьте на это сообщение новыми инструкциями для группы %d в течение 10 минут. Максимум %d символов. Для отмены: /cancel",
			groupID, db.MaxGroupSummaryInstructionsLength,
		))
		if promptID != 0 {
			a.setPendingInstructions(chatID, groupID, promptID)
		}
	case strings.HasPrefix(data, "clear:"):
		groupID, ok := parseInstructionGroupID(strings.TrimPrefix(data, "clear:"))
		if !ok {
			return
		}
		if !a.ensureInstructionsGroupAllowed(ctx, chatID, groupID) {
			return
		}
		if err := a.db.ClearGroupSummaryInstructions(ctx, groupID); err != nil {
			logger.Error().Err(err).Int64("group_id", groupID).Msg("failed to clear group summary instructions")
			a.deps.SendMessage(ctx, chatID, "Ошибка удаления инструкций.")
			return
		}
		a.clearPendingInstructions(chatID)
		a.deps.SendMessage(ctx, chatID, fmt.Sprintf("Инструкции для группы %d очищены.", groupID))
	}
}

func (a *Admin) ensureInstructionsGroupAllowed(ctx context.Context, chatID, groupID int64) bool {
	allowed, err := a.db.IsGroupAllowed(ctx, groupID)
	if err != nil {
		logger.Error().Err(err).Int64("group_id", groupID).Msg("failed to check group allowlist for instructions")
		a.deps.SendMessage(ctx, chatID, "Ошибка проверки группы.")
		return false
	}
	if !allowed {
		a.deps.SendMessage(ctx, chatID, fmt.Sprintf("Группа %d не разрешена для бота.", groupID))
		return false
	}
	return true
}

func (a *Admin) showInstructionsGroup(ctx context.Context, chatID, groupID int64) {
	item, err := a.db.GetGroupSummaryInstructions(ctx, groupID)
	if err != nil {
		logger.Error().Err(err).Int64("group_id", groupID).Msg("failed to get group summary instructions")
		a.deps.SendMessage(ctx, chatID, "Ошибка получения инструкций.")
		return
	}

	text := fmt.Sprintf("*Инструкции для группы %s:*\n\n", summarizer.EscapeMarkdown(fmt.Sprintf("%d", groupID)))
	if item == nil || strings.TrimSpace(item.Instructions) == "" {
		text += "_Не заданы\\._"
	} else {
		text += summarizer.EscapeMarkdown(item.Instructions)
	}

	keyboard := [][]telego.InlineKeyboardButton{
		{
			{Text: "Edit", CallbackData: fmt.Sprintf("inst:edit:%d", groupID)},
			{Text: "Clear", CallbackData: fmt.Sprintf("inst:clear:%d", groupID)},
		},
		{
			{Text: "Cancel", CallbackData: "inst:cancel"},
		},
	}
	a.sendInstructionsKeyboard(ctx, chatID, text, keyboard)
}

func (a *Admin) handlePendingSummaryInstructions(ctx context.Context, msg *telego.Message, cmd string) bool {
	pending, ok := a.pendingInstructionsGroup(msg.Chat.ID)
	if !ok {
		return false
	}

	if cmd == "/cancel" {
		a.clearPendingInstructions(msg.Chat.ID)
		a.deps.SendMessage(ctx, msg.Chat.ID, "Настройка инструкций отменена.")
		return true
	}
	if strings.HasPrefix(cmd, "/") || msg.ReplyToMessage == nil || int64(msg.ReplyToMessage.MessageID) != pending.promptID {
		return false
	}

	groupID := pending.groupID
	if !a.ensureInstructionsGroupAllowed(ctx, msg.Chat.ID, groupID) {
		a.clearPendingInstructions(msg.Chat.ID)
		return true
	}
	if err := a.db.SetGroupSummaryInstructions(ctx, groupID, msg.From.ID, msg.Text); err != nil {
		logger.Error().Err(err).Int64("group_id", groupID).Msg("failed to save group summary instructions")
		a.deps.SendMessage(ctx, msg.Chat.ID, "Ошибка сохранения инструкций. Проверьте длину сообщения.")
		return true
	}

	a.clearPendingInstructions(msg.Chat.ID)
	a.deps.SendMessage(ctx, msg.Chat.ID, fmt.Sprintf("Инструкции для группы %d сохранены.", groupID))
	return true
}

func (a *Admin) sendInstructionsKeyboard(ctx context.Context, chatID int64, text string, rows [][]telego.InlineKeyboardButton) {
	defer a.metrics.TelegramSend.Start()()
	keyboard := &telego.InlineKeyboardMarkup{InlineKeyboard: rows}
	_, err := a.telegram.SendMessage(ctx,
		tu.Message(tu.ID(chatID), text).
			WithParseMode("MarkdownV2").
			WithReplyMarkup(keyboard),
	)
	if err != nil {
		logger.Error().Err(err).Int64("chat_id", chatID).Msg("failed to send instructions keyboard")
		a.metrics.RecordError("telegram_send", err.Error())
	}
}

type pendingInstructions struct {
	groupID, promptID int64
	expires           time.Time
}

func (a *Admin) setPendingInstructions(chatID, groupID, promptID int64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.pendingSummaryInstructions[chatID] = pendingInstructions{groupID: groupID, promptID: promptID, expires: time.Now().Add(10 * time.Minute)}
}

func (a *Admin) clearPendingInstructions(chatID int64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.pendingSummaryInstructions, chatID)
}

func (a *Admin) pendingInstructionsGroup(chatID int64) (pendingInstructions, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	pending, ok := a.pendingSummaryInstructions[chatID]
	if ok && !time.Now().Before(pending.expires) {
		delete(a.pendingSummaryInstructions, chatID)
		return pendingInstructions{}, false
	}
	return pending, ok
}

func parseInstructionGroupID(raw string) (int64, bool) {
	groupID, err := strconv.ParseInt(raw, 10, 64)
	return groupID, err == nil
}

func callbackChatID(cq *telego.CallbackQuery) int64 {
	if cq.Message == nil {
		return 0
	}
	return cq.Message.GetChat().ID
}
