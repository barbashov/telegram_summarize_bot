package handlers

import (
	"context"
	"encoding/json"
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
	dispatch, err := b.acceptUpdate(ctx, update)
	if err != nil {
		logger.Error().Err(err).Int("update_id", update.UpdateID).Msg("failed to accept update")
		return
	}
	if dispatch {
		b.dispatchUpdate(ctx, update)
	}
}

// acceptUpdate persists chat history and returns whether the update needs a worker.
func (b *Bot) acceptUpdate(ctx context.Context, update telego.Update) (bool, error) {
	if update.MyChatMember != nil {
		return update.MyChatMember.Chat.Type != "private", nil
	}
	if update.CallbackQuery != nil {
		return b.cfg.IsAdminUser(update.CallbackQuery.From.ID), nil
	}
	if msg := update.EditedMessage; msg != nil && msg.From != nil && msg.Chat.Type != "private" {
		text, _ := replyTextAndEntities(msg)
		date := msg.EditDate
		if date == 0 {
			date = msg.Date
		}
		if update.UpdateID == 0 {
			b.handleEditedMessage(ctx, msg)
			return false, nil
		}
		return false, b.db.ApplyMessageEdit(ctx, msg.Chat.ID, int64(msg.MessageID), text, date, int64(update.UpdateID))
	}
	msg := update.Message
	if msg == nil || msg.From == nil {
		return false, nil
	}
	text, entities := replyTextAndEntities(msg)
	if text == "" && !hasImageMedia(msg) {
		return false, nil
	}
	if msg.Chat.Type == "private" {
		return true, nil
	}

	groupID := msg.Chat.ID
	if err := b.db.UpsertKnownGroup(ctx, groupID, msg.Chat.Title, msg.Chat.Username); err != nil {
		return false, err
	}
	allowed, err := b.db.IsGroupAllowed(ctx, groupID)
	if err != nil || !allowed {
		return false, err
	}
	var forwardedFrom string
	if msg.ForwardOrigin != nil {
		forwardedFrom = forwardOriginHandle(msg.ForwardOrigin, groupID, b.userHashSalt)
	} else if _, err := b.extractCommandFromMention(text, entities); err == nil {
		return true, nil
	}
	timestamp := time.Now().UTC()
	if msg.Date > 0 {
		timestamp = time.Unix(msg.Date, 0).UTC()
	}
	var replyToTgID int64
	if msg.ReplyToMessage != nil {
		replyToTgID = int64(msg.ReplyToMessage.MessageID)
	}
	return false, b.db.AddMessageWithPhotos(ctx, &db.Message{
		GroupID: groupID, UserHash: db.UserHash(msg.From.ID, groupID, b.userHashSalt),
		Text: text, Timestamp: timestamp, ForwardedFrom: forwardedFrom, TgMessageID: int64(msg.MessageID), ReplyToTgID: replyToTgID,
	}, extractPhotoRecords(msg))
}

func (b *Bot) queueUpdate(ctx context.Context, update telego.Update) error {
	dispatch, err := b.acceptUpdate(ctx, update)
	if err != nil || !dispatch {
		return err
	}
	// Only retain the IDs needed to send setup help to a non-admin.
	if msg := update.Message; msg != nil && msg.Chat.Type == "private" && !b.cfg.IsAdminUser(msg.From.ID) {
		update.Message = &telego.Message{Chat: msg.Chat, From: &telego.User{ID: msg.From.ID}}
	}
	payload, err := json.Marshal(update)
	if err != nil {
		return err
	}
	return b.db.QueueUpdate(ctx, int64(update.UpdateID), payload)
}

func (b *Bot) dispatchUpdate(ctx context.Context, update telego.Update) {
	switch {
	case update.MyChatMember != nil:
		b.handleMyChatMember(ctx, update.MyChatMember)
	case update.CallbackQuery != nil:
		b.admin.HandleCallbackQuery(ctx, update.CallbackQuery)
	case update.Message != nil && update.Message.Chat.Type == "private":
		b.handlePrivateCommand(ctx, update)
	case update.Message != nil:
		allowed, err := b.db.IsGroupAllowed(ctx, update.Message.Chat.ID)
		if err != nil || !allowed {
			return
		}
		text, entities := replyTextAndEntities(update.Message)
		if command, err := b.extractCommandFromMention(text, entities); err == nil {
			b.handleCommand(ctx, update, command)
		}
	}
}

func (b *Bot) handleMyChatMember(ctx context.Context, cmu *telego.ChatMemberUpdated) {
	// Ignore private-chat membership changes: a user blocking/unblocking the bot
	// is not a group join and must not pollute known_groups or notify admins.
	if cmu.Chat.Type == "private" {
		return
	}

	if cmu.NewChatMember == nil {
		return
	}
	newStatus := cmu.NewChatMember.MemberStatus()
	if cmu.OldChatMember != nil {
		oldStatus := cmu.OldChatMember.MemberStatus()
		if oldStatus == "member" || oldStatus == "administrator" {
			return
		}
	}
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
