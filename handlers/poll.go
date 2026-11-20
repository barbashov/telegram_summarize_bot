package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync/atomic"
	"time"

	"github.com/mymmrac/telego"
	"telegram_summarize_bot/logger"
)

const heartbeatPath = "/tmp/telegram_summarize_bot.heartbeat"

func (b *Bot) poll(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var last atomic.Int64
	mark := func() {
		last.Store(time.Now().Unix())
		// File content contains no application data.
		if err := writeHeartbeat(); err != nil {
			logger.Warn().Err(err).Msg("write polling heartbeat")
		}
	}
	mark()
	watchdog := make(chan struct{})
	go func() {
		defer close(watchdog)
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-ticker.C:
				if now.Unix()-last.Load() >= 180 {
					cancel()
					return
				}
			}
		}
	}()

	params := &telego.GetUpdatesParams{Timeout: 50, AllowedUpdates: []string{
		"message", "edited_message", "my_chat_member", "callback_query",
	}}
	for ctx.Err() == nil {
		updates, err := b.telegram.GetUpdates(ctx, params)
		if err == nil {
			if len(updates) == 0 {
				params.Offset = 0
			}
			for _, update := range updates {
				// Finish a bounded write when shutdown arrives during ingestion.
				writeCtx, stop := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
				err = b.queueUpdate(writeCtx, update)
				stop()
				if err != nil {
					break
				}
				params.Offset = update.UpdateID + 1
				if ctx.Err() != nil {
					break
				}
			}
		}
		if ctx.Err() != nil {
			break
		}
		mark()
		if err != nil {
			logger.Warn().Msg("Telegram polling or update persistence failed; retrying")
			sleepCtx(ctx, 3*time.Second)
		}
	}
	<-watchdog
	if time.Now().Unix()-last.Load() >= 180 {
		return fmt.Errorf("telegram polling watchdog timed out")
	}
	return nil
}

func (b *Bot) updateWorker(ctx context.Context) {
	for ctx.Err() == nil {
		jobs, err := b.db.PendingUpdates(ctx, 64)
		if err != nil {
			logger.Error().Err(err).Msg("read Telegram command queue")
		}
		for _, job := range jobs {
			var update telego.Update
			if err := json.Unmarshal(job.Payload, &update); err != nil {
				logger.Error().Int64("update_id", job.ID).Msg("invalid Telegram command in queue")
				_ = b.db.FinishUpdate(ctx, job.ID)
				continue
			}
			chatID := updateChatID(update)
			if _, busy := b.activeUpdates.LoadOrStore(chatID, struct{}{}); busy {
				continue
			}
			select {
			case b.sem <- struct{}{}:
			case <-ctx.Done():
				b.activeUpdates.Delete(chatID)
				return
			}
			b.inflight.Go(func() {
				defer b.activeUpdates.Delete(chatID)
				defer func() { <-b.sem }()
				b.dispatchUpdate(context.WithValue(ctx, commandIDKey{}, job.ID), update)
				if ctx.Err() == nil {
					if err := b.db.FinishUpdate(ctx, job.ID); err != nil && !errors.Is(err, context.Canceled) {
						logger.Error().Err(err).Msg("finish Telegram command")
					}
				}
			})
		}

		if err := b.db.PruneUpdates(ctx); err != nil && ctx.Err() == nil {
			logger.Error().Err(err).Msg("purge expired Telegram commands")
		}
		sleepCtx(ctx, time.Second)
	}
}

func updateChatID(update telego.Update) int64 {
	switch {
	case update.Message != nil:
		return update.Message.Chat.ID
	case update.MyChatMember != nil:
		return update.MyChatMember.Chat.ID
	case update.CallbackQuery != nil:
		if update.CallbackQuery.Message != nil {
			return update.CallbackQuery.Message.GetChat().ID
		}
		return update.CallbackQuery.From.ID
	default:
		return 0
	}
}

func writeHeartbeat() error {
	file, err := os.CreateTemp("/tmp", "telegram-summarize-heartbeat-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(file.Name()) }()
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(file.Name(), heartbeatPath)
}
