package admin

func (a *Admin) handleHelp(chatID int64) {
	helpText := "*Команды администратора*\n\n" +
		"`/help` — показать это сообщение\n" +
		"`/status` — статус бота и метрики\n" +
		"`/reset` — сбросить все метрики\n" +
		"`/groups` — список разрешённых групп\n" +
		"`/groups add <group_id>` — добавить группу\n" +
		"`/groups remove <group_id>` — удалить группу\n\n" +
		"*Суммаризация URL:*\nОтправьте ссылку — бот загрузит страницу и вернёт краткое содержание\\."
	a.deps.SendFormatted(chatID, helpText)
}
