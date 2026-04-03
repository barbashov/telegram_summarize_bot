package handlers

import (
	"github.com/mymmrac/telego"
)

func (b *Bot) handleHelp(update telego.Update) {
	msg := update.Message
	if msg == nil {
		return
	}

	helpText := "📖 *Доступные команды:*\n\n" +
		"• `summarize [часы]` \\(или `s`, `sub`\\) — суммировать сообщения за последние N часов \\(по умолчанию 24\\)\n" +
		"• `schedule` — показать расписание ежедневной сводки\n" +
		"• `help` — показать это сообщение\n\n" +
		"_Примеры: @bot summarize, @bot summarize 12_"

	if b.isGroupAdmin(msg.Chat.ID, msg.From.ID) {
		helpText += "\n\n*Команды администратора:*\n" +
			"• `schedule on` — включить ежедневную сводку\n" +
			"• `schedule off` — выключить ежедневную сводку\n" +
			"• `schedule ЧЧ:ММ` — установить время ежедневной сводки в UTC\n" +
			"• `schedule now` — запустить внеплановую сводку прямо сейчас\n\n" +
			"_Пример: @bot schedule 08:00_"
	}

	b.sendFormatted(msg.Chat.ID, helpText)
}

func (b *Bot) handlePrivateChatInfo(update telego.Update) {
	msg := update.Message
	if msg == nil {
		return
	}

	privateInfoText := "Я работаю только в группах и полезен для суммаризации групповых обсуждений\\.\n\n" +
		"Добавьте меня в группу и используйте `@" + b.username + " summarize`\\."

	b.sendFormatted(msg.Chat.ID, privateInfoText)
}
