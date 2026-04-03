package handlers

import (
	"telegram_summarize_bot/logger"

	"github.com/mymmrac/telego"
	tu "github.com/mymmrac/telego/telegoutil"
)

func (b *Bot) isGroupAdmin(groupID, userID int64) bool {
	member, err := b.telegram.GetChatMember(&telego.GetChatMemberParams{
		ChatID: tu.ID(groupID),
		UserID: userID,
	})
	if err != nil {
		logger.Error().Err(err).Msg("failed to get chat member")
		return false
	}
	status := member.MemberStatus()
	return status == "creator" || status == "administrator"
}
