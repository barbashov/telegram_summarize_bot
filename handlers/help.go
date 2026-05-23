package handlers

import (
	"context"

	"github.com/mymmrac/telego"
)

func (b *Bot) handleHelp(ctx context.Context, update telego.Update) {
	msg := update.Message
	if msg == nil {
		return
	}

	helpText := "📖 *Доступные команды:*\n\n" +
		"• `summarize [часы]` \\(или `s`, `sub`\\) — суммировать сообщения за последние N часов \\(по умолчанию 24\\)\n" +
		"• *Ответ* на сообщение с упоминанием бота — разобрать именно его \\(ссылку, изображение или текст\\); слово `summarize` необязательно\\. Можно добавить запрос, например `@bot опиши мем` или `@bot как это можно использовать`\n" +
		"• `schedule` — показать расписание ежедневной сводки\n" +
		"• `help` — показать это сообщение\n\n" +
		"_Примеры: @bot summarize, @bot summarize 12, ответом — @bot опиши мем_"

	if b.isGroupAdmin(ctx, msg.Chat.ID, msg.From.ID) {
		helpText += "\n\n*Команды администратора:*\n" +
			"• `schedule on` — включить ежедневную сводку\n" +
			"• `schedule off` — выключить ежедневную сводку\n" +
			"• `schedule ЧЧ:ММ` — установить время ежедневной сводки в UTC\n" +
			"• `schedule now` — запустить внеплановую сводку прямо сейчас\n\n" +
			"_Пример: @bot schedule 08:00_"
	}

	b.sendFormatted(ctx, msg.Chat.ID, helpText)
}

func (b *Bot) handlePrivateChatInfo(ctx context.Context, update telego.Update) {
	msg := update.Message
	if msg == nil {
		return
	}

	privateInfoText := "Я работаю только в группах и полезен для суммаризации групповых обсуждений\\.\n\n" +
		"Добавьте меня в группу и используйте `@" + b.username + " summarize`\\."

	b.sendFormatted(ctx, msg.Chat.ID, privateInfoText)
}
