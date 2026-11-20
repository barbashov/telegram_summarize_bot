package handlers

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/barbashov/telegramify-markdown-go"
	"github.com/mymmrac/telego/telegoapi"
	tu "github.com/mymmrac/telego/telegoutil"
	"telegram_summarize_bot/db"
	"telegram_summarize_bot/logger"
)

type commandIDKey struct{}

func deliveryKey(ctx context.Context, chatID, statusID int64) string {
	if id, ok := ctx.Value(commandIDKey{}).(int64); ok {
		return fmt.Sprintf("update:%d", id)
	}
	return fmt.Sprintf("reply:%d:%d", chatID, statusID)
}

func (b *Bot) DeliverSummary(ctx context.Context, chatID, statusID int64, markdown string) {
	b.deliver(ctx, db.Delivery{Key: deliveryKey(ctx, chatID, statusID), ChatID: chatID, StatusID: statusID}, markdown)
}

func (b *Bot) deliver(ctx context.Context, delivery db.Delivery, markdown string) bool {
	delivery.UpdateID, _ = ctx.Value(commandIDKey{}).(int64)
	for _, chunk := range telegramify.RenderChunks(markdown, telegramMessageLimit) {
		delivery.Parts = append(delivery.Parts, db.DeliveryPart{Markdown: chunk.MarkdownV2, Text: chunk.Text})
	}
	if err := b.db.SaveDelivery(ctx, delivery); err != nil {
		logger.Error().Err(err).Msg("failed to persist summary")
		return false
	}
	return b.resumeDelivery(ctx, delivery.Key)
}

func (b *Bot) resumeDelivery(ctx context.Context, key string) bool {
	if _, busy := b.deliveries.LoadOrStore(key, struct{}{}); busy {
		return false
	}
	defer b.deliveries.Delete(key)
	d, err := b.db.GetDelivery(ctx, key)
	if err != nil || d == nil {
		return false
	}
	if d.NextAt > time.Now().Unix() || d.Daily != "" && d.Attempts >= scheduledMaxAttempts {
		return false
	}
	if d.Group {
		allowed, err := b.db.IsGroupAllowed(ctx, d.ChatID)
		if err != nil {
			return false
		}
		if !allowed {
			_ = b.db.CancelDelivery(ctx, key)
			return false
		}
	}
	for d.NextPart < len(d.Parts) {
		if err := b.sendDeliveryPart(ctx, d); err != nil {
			if ctx.Err() != nil {
				return false
			}
			var apiErr *telegoapi.Error
			delay := min(30*time.Second<<uint(min(d.Attempts, 6)), 30*time.Minute)
			maxAttempts := 8
			if d.Daily != "" {
				delay = scheduledRetryDelay
				maxAttempts = scheduledMaxAttempts
			}
			if errors.As(err, &apiErr) && apiErr.Parameters != nil {
				delay = max(delay, time.Duration(apiErr.Parameters.RetryAfter)*time.Second)
			}
			if d.Attempts+1 >= maxAttempts {
				if err := b.db.FailDelivery(ctx, d); err != nil {
					logger.Error().Err(err).Msg("record exhausted summary delivery")
				}
				return false
			}
			if err := b.db.DelayDelivery(ctx, key, time.Now().Add(delay)); err != nil {
				logger.Error().Err(err).Msg("reschedule summary delivery")
			}
			return false
		}
		if err := b.db.ConfirmPart(ctx, d); err != nil {
			logger.Error().Err(err).Msg("confirm summary delivery")
			return false
		}
		d.NextPart++
	}
	return true
}

func (b *Bot) sendDeliveryPart(ctx context.Context, d *db.Delivery) error {
	part := d.Parts[d.NextPart]
	if d.NextPart == 0 && d.StatusID != 0 {
		err := b.editFormatted(ctx, d.ChatID, d.StatusID, part.Markdown)
		var apiErr *telegoapi.Error
		if err == nil || (errors.As(err, &apiErr) && strings.Contains(apiErr.Description, "message is not modified")) {
			return nil
		}
		if !errors.As(err, &apiErr) || apiErr.ErrorCode != 400 {
			return err
		}
		if part.Text != "" && strings.Contains(apiErr.Description, "parse entities") {
			err := b.editMessage(ctx, d.ChatID, d.StatusID, part.Text)
			if errors.As(err, &apiErr) && strings.Contains(apiErr.Description, "message is not modified") {
				return nil
			}
			return err
		}
	}
	err := b.sendFormattedFinal(ctx, d.ChatID, part.Markdown)
	var apiErr *telegoapi.Error
	if part.Text != "" && errors.As(err, &apiErr) && apiErr.ErrorCode == 400 && strings.Contains(apiErr.Description, "parse entities") {
		_, err = b.telegram.SendMessage(ctx, tu.Message(tu.ID(d.ChatID), part.Text))
	}
	return err
}

func (b *Bot) deliveryWorker(ctx context.Context) {
	for ctx.Err() == nil {
		if err := b.db.ExpireDeliveries(ctx); err != nil {
			logger.Error().Err(err).Msg("purge expired deliveries")
		}
		keys, err := b.db.DueDeliveries(ctx)
		if err != nil {
			logger.Error().Err(err).Msg("load due summary deliveries")
		}
		for _, key := range keys {
			b.resumeDelivery(ctx, key)
		}
		sleepCtx(ctx, 3*time.Second)
	}
}
