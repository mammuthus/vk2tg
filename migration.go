package main

import (
	"context"
	"errors"
	"net/url"
	"strconv"
	"time"
)

type VKHistoryPage struct {
	Count int         `json:"count"`
	Items []VKMessage `json:"items"`
}

func (client *VKClient) GetHistoryPage(ctx context.Context, peerID int64, offset, count int) (VKHistoryPage, error) {
	if offset < 0 || count < 1 || count > 100 {
		return VKHistoryPage{}, ErrVKInvalidResponse
	}
	var page VKHistoryPage
	parameters := url.Values{"peer_id": {strconv.FormatInt(peerID, 10)}, "offset": {strconv.Itoa(offset)}, "count": {strconv.Itoa(count)}, "rev": {"1"}}
	if err := client.call(ctx, "messages.getHistory", parameters, &page); err != nil {
		return VKHistoryPage{}, err
	}
	if page.Count < 0 || page.Items == nil || len(page.Items) > count || len(page.Items) > page.Count {
		return VKHistoryPage{}, ErrVKInvalidResponse
	}
	var previous int64
	for _, message := range page.Items {
		if message.ID <= previous || message.PeerID != peerID || message.FromID == 0 {
			return VKHistoryPage{}, ErrVKInvalidResponse
		}
		previous = message.ID
	}
	return page, nil
}

func (relay *Relay) MigrateHistory(ctx context.Context) error {
	if relay.config.DryRun {
		return errors.New("migrate-history requires DRY_RUN=false; use offline tests before migration")
	}
	if relay.store == nil {
		return errors.New("migration state database is required")
	}
	ctx = relay.debugContext(ctx, "mode", "migration")
	clock := relay.migrationClock
	if clock == nil {
		clock = realVKClock{}
	}
	state, err := relay.store.loadMigration(ctx, relay.config.VKTargetPeerID, relay.config.TelegramTargetChatID)
	if err != nil {
		return err
	}
	if state.PendingID != 0 {
		identifier, err := relay.store.Lookup(ctx, state.PendingID)
		if err != nil {
			return err
		}
		if identifier == 0 {
			relay.logger.Error("migration ambiguous delivery; manual reconciliation required", "vk_message_id", state.PendingID)
			return errors.New("pending Telegram delivery has no mapping; refusing automatic resend")
		}
		state.LastID, state.PendingID = state.PendingID, 0
		state.Offset++
		state.Sent++
		state.NotBefore = clock.Now().Add(time.Duration(state.PauseSeconds) * time.Second).UnixNano()
		if err := relay.store.saveMigration(ctx, state); err != nil {
			return err
		}
	}
	if state.Completed {
		relay.logger.Info("migration already complete", "total", state.Total, "sent", state.Sent, "skipped", state.Skipped, "batches", state.Batches)
		return nil
	}
	for {
		if delay := time.Unix(0, state.NotBefore).Sub(clock.Now()); delay > 0 {
			relay.logger.Info("migration batch pause", "delay", delay, "next_batch", state.Batches+1)
			if err := clock.Wait(ctx, delay); err != nil {
				return err
			}
		}
		started := clock.Now()
		vkBefore, telegramBefore := relay.vk.stats.snapshot(), relay.telegram.stats.snapshot()
		offset, count := state.Offset, 50
		if offset > 0 {
			offset--
			count++
		}
		page, err := relay.vk.GetHistoryPage(ctx, relay.config.VKTargetPeerID, offset, count)
		if err != nil {
			traceFailure(ctx, "migration history page", err)
			return err
		}
		if page.Count < state.Total {
			return errors.New("VK history shrank during migration; refusing shifted pagination")
		}
		if state.Offset > 0 {
			if len(page.Items) == 0 || page.Items[0].ID != state.LastID {
				return errors.New("VK history pagination boundary changed")
			}
			page.Items = page.Items[1:]
		}
		state.Total = page.Count
		if len(page.Items) == 0 && state.Offset < state.Total {
			return errors.New("VK history returned an incomplete page")
		}
		if state.Offset+len(page.Items) > state.Total {
			return ErrVKInvalidResponse
		}
		if state.Batches == 0 {
			relay.logger.Info("migration history selected", "total", state.Total, "batch_size", 50, "estimated_batches", (state.Total+49)/50, "minimum_pause_seconds", 60)
		}
		if len(page.Items) == 0 {
			break
		}
		state.Batches++
		if err := relay.store.saveMigration(ctx, state); err != nil {
			return err
		}
		sentBefore, skippedBefore := state.Sent, state.Skipped
		batchPause := state.PauseSeconds
		relay.logger.Info("migration batch started", "batch", state.Batches, "first_vk_id", page.Items[0].ID, "last_vk_id", page.Items[len(page.Items)-1].ID, "total", state.Total, "offset", state.Offset)
		batchErr := func() error {
			for _, message := range page.Items {
				messageCtx := relay.debugContext(ctx, "vk_message_id", message.ID, "batch_id", state.Batches)
				identifier, err := relay.store.Lookup(messageCtx, message.ID)
				if err != nil {
					return err
				}
				accepted := false
				var rendered RenderedMessage
				if identifier == 0 && relay.acceptsContext(messageCtx, message) {
					rendered, accepted, err = relay.prepareMessage(messageCtx, message)
					if err != nil {
						traceFailure(messageCtx, "migration preparation", err)
						return err
					}
				}
				if accepted {
					state.PendingID = message.ID
					if err := relay.store.saveMigration(ctx, state); err != nil {
						return err
					}
					if err := relay.deliverMapped(messageCtx, rendered); err != nil {
						traceFailure(messageCtx, "migration delivery", err)
						return err
					}
					state.Sent++
				} else {
					state.Skipped++
				}
				state.PendingID = 0
				state.Offset++
				state.LastID = message.ID
				if relay.telegram.stats.snapshot().RateLimits > telegramBefore.RateLimits {
					state.PauseSeconds = batchPause + 60
				}
				state.NotBefore = clock.Now().Add(time.Duration(state.PauseSeconds) * time.Second).UnixNano()
				if err := relay.store.saveMigration(ctx, state); err != nil {
					return err
				}
				relay.logger.Info("migration message checkpoint", "vk_message_id", message.ID, "sent", accepted, "already_mapped", identifier != 0, "processed", state.Offset, "total", state.Total)
			}
			return nil
		}()
		failed := 0
		if batchErr != nil {
			failed = 1
		}
		relay.logger.Info("migration batch complete", "batch", state.Batches, "first_vk_id", page.Items[0].ID, "last_vk_id", state.LastID,
			"processed", state.Sent+state.Skipped-sentBefore-skippedBefore, "sent", state.Sent-sentBefore, "skipped", state.Skipped-skippedBefore, "failed", failed,
			"vk", relay.vk.stats.snapshot().since(vkBefore), "telegram", relay.telegram.stats.snapshot().since(telegramBefore), "duration", clock.Now().Sub(started), "pause_seconds", state.PauseSeconds)
		if batchErr != nil {
			return batchErr
		}
		if state.Offset == state.Total {
			break
		}
	}
	state.Completed = true
	if err := relay.store.saveMigration(ctx, state); err != nil {
		return err
	}
	relay.logger.Info("migration complete", "total", state.Total, "sent", state.Sent, "skipped", state.Skipped, "failed", 0, "batches", state.Batches, "vk", relay.vk.stats.snapshot(), "telegram", relay.telegram.stats.snapshot())
	return nil
}
