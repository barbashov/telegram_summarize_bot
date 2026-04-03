package admin

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"telegram_summarize_bot/logger"
	"telegram_summarize_bot/summarizer"
)

func (a *Admin) handleGroups(ctx context.Context, chatID, userID int64, args []string) {
	if len(args) == 0 {
		a.sendGroupsList(ctx, chatID)
		return
	}

	subCmd := strings.ToLower(args[0])

	switch subCmd {
	case "add":
		if len(args) < 2 {
			a.deps.SendFormatted(chatID, "Использование: `/groups add <group_id>`")
			return
		}
		groupID, err := strconv.ParseInt(args[1], 10, 64)
		if err != nil {
			a.deps.SendMessage(chatID, "Неверный ID группы.")
			return
		}
		// Best-effort title lookup; don't block add if group is unknown.
		title := fmt.Sprintf("%d", groupID)
		groups, err := a.db.GetKnownGroups(ctx)
		if err != nil {
			logger.Error().Err(err).Msg("failed to get known groups")
		} else {
			for i := range groups {
				if groups[i].GroupID == groupID {
					title = groups[i].Title
					break
				}
			}
		}
		if err := a.db.AddAllowedGroup(ctx, groupID, userID); err != nil {
			logger.Error().Err(err).Msg("failed to add allowed group")
			a.deps.SendMessage(chatID, "Ошибка добавления группы.")
			return
		}
		a.deps.SendMessage(chatID, fmt.Sprintf("✅ %s добавлена.", title))
	case "remove":
		if len(args) < 2 {
			a.deps.SendFormatted(chatID, "Использование: `/groups remove <group_id>`")
			return
		}
		groupID, err := strconv.ParseInt(args[1], 10, 64)
		if err != nil {
			a.deps.SendMessage(chatID, "Неверный ID группы.")
			return
		}
		groups, err := a.db.GetKnownGroups(ctx)
		if err != nil {
			logger.Error().Err(err).Msg("failed to get known groups")
			a.deps.SendMessage(chatID, "Ошибка получения списка групп.")
			return
		}
		var foundTitle string
		for i := range groups {
			if groups[i].GroupID == groupID {
				foundTitle = groups[i].Title
				break
			}
		}
		if foundTitle == "" {
			a.deps.SendMessage(chatID, fmt.Sprintf("Группа %d не найдена в списке известных групп.", groupID))
			a.sendGroupsList(ctx, chatID)
			return
		}
		if err := a.db.RemoveAllowedGroup(ctx, groupID); err != nil {
			logger.Error().Err(err).Msg("failed to remove allowed group")
			a.deps.SendMessage(chatID, "Ошибка удаления группы.")
			return
		}
		a.deps.SendMessage(chatID, fmt.Sprintf("❌ %s удалена.", foundTitle))
	default:
		a.deps.SendFormatted(chatID, "Неизвестная подкоманда\\. Используйте: `/groups`, `/groups add <id>`, `/groups remove <id>`")
	}
}

func (a *Admin) sendGroupsList(ctx context.Context, chatID int64) {
	groups, err := a.db.GetKnownGroups(ctx)
	if err != nil {
		logger.Error().Err(err).Msg("failed to get known groups")
		a.deps.SendMessage(chatID, "Ошибка получения списка групп.")
		return
	}
	if len(groups) == 0 {
		a.deps.SendMessage(chatID, "Нет известных групп.")
		return
	}

	var sb strings.Builder
	sb.WriteString("📋 *Известные группы:*\n\n")
	for _, g := range groups {
		status := "❌"
		if g.Allowed {
			status = "✅"
		}
		escapedTitle := summarizer.EscapeMarkdown(g.Title)
		title := escapedTitle
		if g.Username != "" {
			title = fmt.Sprintf("[%s](https://t.me/%s)", escapedTitle, g.Username)
		} else if g.GroupID < 0 {
			// Supergroup/channel: strip the -100 prefix for the t.me/c/ link
			chatID := (-g.GroupID) - 1_000_000_000_000
			if chatID > 0 {
				title = fmt.Sprintf("[%s](https://t.me/c/%d)", escapedTitle, chatID)
			}
		}
		fmt.Fprintf(&sb, "%s %s \\(%s\\)\n", status, title, summarizer.EscapeMarkdown(fmt.Sprintf("%d", g.GroupID)))
	}
	sb.WriteString("\nДля управления:\n• `/groups add <group_id>`\n• `/groups remove <group_id>`")
	a.deps.SendFormatted(chatID, sb.String())
}
