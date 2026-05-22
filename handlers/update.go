package handlers

import (
	"context"
	"fmt"
	"time"

	"telegram_summarize_bot/db"
	"telegram_summarize_bot/logger"

	"github.com/mymmrac/telego"
)

// forwardOriginHandle returns a pseudonymized "<kind>:<8 hex>" identifier for
// the original author of a forwarded message. The handle is stable per
// (origin, groupID) so the summarizer can still cluster repeat forwards from
// the same source without storing the underlying username, real name, or
// channel title. Returns "" when no stable identifier is available.
func forwardOriginHandle(origin telego.MessageOrigin, groupID int64, salt []byte) string {
	switch o := origin.(type) {
	case *telego.MessageOriginUser:
		return "user:" + db.UserHash(o.SenderUser.ID, groupID, salt)
	case *telego.MessageOriginHiddenUser:
		if o.SenderUserName == "" {
			return ""
		}
		return "hidden:" + db.HashString(o.SenderUserName, groupID, salt)
	case *telego.MessageOriginChat:
		return "chat:" + db.UserHash(o.SenderChat.ID, groupID, salt)
	case *telego.MessageOriginChannel:
		return "channel:" + db.UserHash(o.Chat.ID, groupID, salt)
	default:
		return ""
	}
}

func (b *Bot) handleUpdate(ctx context.Context, update telego.Update) {
	// Handle bot membership changes (bot added to / removed from a group).
	if update.MyChatMember != nil {
		b.handleMyChatMember(ctx, update.MyChatMember)
		return
	}

	if update.CallbackQuery != nil {
		b.admin.HandleCallbackQuery(ctx, update.CallbackQuery)
		return
	}

	if update.Message == nil {
		return
	}

	msg := update.Message
	if msg.From == nil {
		return
	}

	groupID := msg.Chat.ID
	text := msg.Text
	tgMessageID := int64(msg.MessageID)

	var replyToTgID int64
	if msg.ReplyToMessage != nil {
		replyToTgID = int64(msg.ReplyToMessage.MessageID)
	}

	logger.Debug().
		Int64("group_id", groupID).
		Str("text", text).
		Msg("Received message")

	if text == "" {
		return
	}

	if msg.Chat.Type != "private" {
		// Track group title even for non-allowed groups.
		if err := b.db.UpsertKnownGroup(ctx, groupID, msg.Chat.Title, msg.Chat.Username); err != nil {
			logger.Error().Err(err).Int64("group_id", groupID).Msg("failed to upsert known group")
		} else {
			logger.Debug().Int64("group_id", groupID).Str("title", msg.Chat.Title).Str("username", msg.Chat.Username).Msg("upserted known group")
		}
		allowed, err := b.db.IsGroupAllowed(ctx, groupID)
		if err != nil {
			logger.Error().Err(err).Int64("group_id", groupID).Msg("failed to check group allowlist")
			return
		}
		if !allowed {
			logger.Warn().
				Int64("group_id", groupID).
				Str("chat_type", msg.Chat.Type).
				Msg("ignoring message from non-allowed group")
			return
		}
	}

	// Forwarded messages are stored with original author attribution but never
	// treated as commands — the forwarder didn't intend to issue one.
	if msg.ForwardOrigin != nil {
		forwardedFrom := forwardOriginHandle(msg.ForwardOrigin, groupID, b.userHashSalt)
		if err := b.db.AddMessage(ctx, &db.Message{
			GroupID:       groupID,
			UserHash:      db.UserHash(msg.From.ID, groupID, b.userHashSalt),
			Text:          text,
			Timestamp:     time.Now(),
			ForwardedFrom: forwardedFrom,
			TgMessageID:   tgMessageID,
			ReplyToTgID:   replyToTgID,
		}); err != nil {
			logger.Error().Err(err).Msg("failed to add forwarded message")
		}
		return
	}

	if msg.Chat.Type == "private" {
		b.handlePrivateCommand(ctx, update)
		return
	}

	command, err := b.extractCommandFromMention(text, msg.Entities)
	if err == nil {
		b.handleCommand(ctx, update, command)
		return
	}

	if err := b.db.AddMessage(ctx, &db.Message{
		GroupID:     groupID,
		UserHash:    db.UserHash(msg.From.ID, groupID, b.userHashSalt),
		Text:        text,
		Timestamp:   time.Now(),
		TgMessageID: tgMessageID,
		ReplyToTgID: replyToTgID,
	}); err != nil {
		logger.Error().Err(err).Msg("failed to add message")
	}
}

func (b *Bot) handleMyChatMember(ctx context.Context, cmu *telego.ChatMemberUpdated) {
	newStatus := cmu.NewChatMember.MemberStatus()
	if newStatus != "member" && newStatus != "administrator" {
		return
	}

	groupID := cmu.Chat.ID
	title := cmu.Chat.Title

	if err := b.db.UpsertKnownGroup(ctx, groupID, title, cmu.Chat.Username); err != nil {
		logger.Error().Err(err).Int64("group_id", groupID).Msg("failed to upsert known group on bot join")
	} else {
		logger.Info().Int64("group_id", groupID).Str("title", title).Str("username", cmu.Chat.Username).Msg("upserted known group on bot join")
	}

	msg := fmt.Sprintf("Бот добавлен в группу «%s» (%d).\nДля разрешения: /groups add %d", title, groupID, groupID)
	b.NotifyUsers(ctx, msg)
}

func (b *Bot) handlePrivateCommand(ctx context.Context, update telego.Update) {
	msg := update.Message
	if msg == nil {
		return
	}

	isAdmin := b.cfg.IsAdminUser(msg.From.ID)

	if isAdmin {
		if b.admin.Handle(ctx, update) {
			return
		}
	}

	b.handlePrivateChatInfo(ctx, update)
}
