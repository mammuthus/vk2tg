package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

type Relay struct {
	config         Config
	vk             *VKClient
	telegram       *TelegramClient
	mediaHTTP      *http.Client
	logger         *slog.Logger
	tempRoot       string
	retryDelay     time.Duration
	senderNames    map[int64]string
	store          *MessageStore
	migrationClock vkClock
}

func (relay *Relay) Run(ctx context.Context) error {
	ctx = relay.debugContext(ctx)
	trace(ctx, "relay debug enabled", "dry_run", relay.config.DryRun)
	if relay.retryDelay <= 0 {
		relay.retryDelay = 2 * time.Second
	}
	relay.senderNames = make(map[int64]string)
	var server VKLongPollServer
	var batchID uint64
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if server.Key == "" {
			fresh, err := relay.vk.GetLongPollServer(ctx)
			if err != nil {
				if !retryableVK(err) {
					return err
				}
				if err := relay.retry(ctx); err != nil {
					return err
				}
				continue
			}
			if server.TS != "" {
				fresh.TS = server.TS
			}
			server = fresh
			trace(ctx, "long poll cursor acquired", "ts", safeTS(server.TS))
			relay.logger.Info("vk long poll connected")
		}
		batch, err := relay.vk.WaitLongPoll(ctx, server)
		if err != nil {
			traceFailure(ctx, "long poll wait", err, "ts", safeTS(server.TS))
			if !retryableVK(err) && !errors.Is(err, ErrVKInvalidJSON) && !errors.Is(err, ErrVKInvalidResponse) {
				return err
			}
			if err := relay.retry(ctx); err != nil {
				return err
			}
			continue
		}
		batchID++
		batchCtx := relay.debugContext(ctx, "batch_id", batchID, "ts_before", safeTS(server.TS), "ts_after", safeTS(batch.TS))
		trace(batchCtx, "long poll batch received", "updates", len(batch.Updates), "failed", batch.Failed)
		switch batch.Failed {
		case 0:
		case 1:
			server.TS = batch.TS
			trace(batchCtx, "long poll ts advanced", "reason", "failed_1")
			continue
		case 2:
			server.Key = ""
			trace(batchCtx, "long poll key refresh", "ts_preserved", true)
			continue
		case 3:
			trace(batchCtx, "long poll cursor reset", "reason", "failed_3")
			relay.logger.Warn("long poll cursor expired; resuming from current events")
			server = VKLongPollServer{}
			continue
		default:
			return errors.New("unsupported vk long poll protocol response")
		}
		for index, event := range batch.Updates {
			eventCtx := eventContext(relay.debugContext(batchCtx, "event_index", index), event)
			var message RenderedMessage
			var accepted bool
			for {
				message, accepted, err = relay.prepare(eventCtx, event)
				if err == nil {
					break
				}
				if !retryableVK(err) {
					traceFailure(eventCtx, "batch processing", err, "ts_advanced", false)
					return err
				}
				traceFailure(eventCtx, "message preparation retry", err)
				if err := relay.retry(ctx); err != nil {
					return err
				}
			}
			if !accepted {
				continue
			}
			if relay.config.DryRun {
				trace(eventCtx, "delivery skipped", "reason", "dry_run")
				relay.logger.Info("would relay message", "media_count", len(message.Media), "repost", message.Repost)
				continue
			}
			if err := relay.deliverMapped(eventCtx, message); err != nil {
				traceFailure(eventCtx, "batch processing", err, "ts_advanced", false)
				return err
			}
			relay.logger.Info("message relayed", "media_count", len(message.Media), "repost", message.Repost)
		}
		server.TS = batch.TS
		trace(batchCtx, "long poll batch complete", "updates", len(batch.Updates))
		trace(batchCtx, "long poll ts advanced", "reason", "batch_complete")
		relay.logger.Info("vk long poll cycle complete", "updates", len(batch.Updates))
	}
}

func (relay *Relay) accepts(message VKMessage) bool {
	return relay.acceptsContext(context.Background(), message)
}

func (relay *Relay) acceptsContext(ctx context.Context, message VKMessage) bool {
	_, blocked := relay.config.VKBlockedSenderIDs[message.FromID]
	for _, check := range []struct {
		reason string
		passed bool
	}{
		{"missing_message_id", message.ID != 0},
		{"wrong_peer", message.PeerID == relay.config.VKTargetPeerID},
		{"missing_sender", message.FromID != 0},
		{"blocked_sender", !blocked},
	} {
		trace(ctx, "filter decision", "predicate", check.reason, "passed", check.passed)
		if !check.passed {
			trace(ctx, "message skipped", "reason", check.reason)
			return false
		}
	}
	trace(ctx, "message filters accepted")
	return true
}

func (relay *Relay) prepare(ctx context.Context, event json.RawMessage) (RenderedMessage, bool, error) {
	ctx = eventContext(relay.debugContext(ctx), event)
	message, needsFull, err := decodeLongPollMessageContext(ctx, event)
	if err != nil {
		traceFailure(ctx, "message parsing", err)
		return RenderedMessage{}, false, err
	}
	if !relay.acceptsContext(debugContext(ctx, nil, "stage", "initial_filters"), message) {
		return RenderedMessage{}, false, nil
	}
	if relay.store != nil {
		identifier, err := relay.store.Lookup(ctx, message.ID)
		if err != nil {
			return RenderedMessage{}, false, err
		}
		if identifier != 0 {
			trace(ctx, "message skipped", "reason", "duplicate_mapping", "telegram_message_id", identifier)
			return RenderedMessage{}, false, nil
		}
	}
	if needsFull {
		trace(ctx, "getById started")
		message, err = relay.vk.GetMessageByID(ctx, message.ID)
		if err != nil {
			traceFailure(ctx, "getById", err)
			return RenderedMessage{}, false, err
		}
		traceMessage(ctx, "getById succeeded", message)
		if !relay.acceptsContext(debugContext(ctx, nil, "stage", "enriched_filters"), message) {
			return RenderedMessage{}, false, nil
		}
	}
	return relay.prepareMessage(ctx, message)
}

func (relay *Relay) deliverMapped(ctx context.Context, message RenderedMessage) error {
	ctx = relay.debugContext(ctx, "vk_message_id", message.VKMessageID)
	if relay.config.DryRun {
		return nil
	}
	if relay.store == nil {
		return errors.New("state database is not open")
	}
	existing, err := relay.store.Lookup(ctx, message.VKMessageID)
	if err != nil {
		return err
	}
	if existing != 0 {
		trace(ctx, "delivery skipped", "reason", "duplicate_mapping", "telegram_message_id", existing)
		return nil
	}
	if message.VKReplyToID > 0 {
		message.TelegramReplyID, err = relay.store.Lookup(ctx, message.VKReplyToID)
		if err != nil {
			return err
		}
	}
	trace(ctx, "reply mapping resolved", "reply_vk_id", message.VKReplyToID, "reply_telegram_id", message.TelegramReplyID)
	canonical, err := deliverMessage(ctx, relay.telegram, relay.mediaHTTP, relay.tempRoot, message)
	if err != nil {
		traceFailure(ctx, "delivery", err)
		return err
	}
	trace(ctx, "canonical telegram message", "telegram_message_id", canonical)
	return relay.store.Save(ctx, message.VKMessageID, canonical)
}

func (relay *Relay) prepareMessage(ctx context.Context, message VKMessage) (RenderedMessage, bool, error) {
	ctx = relay.debugContext(ctx, "vk_message_id", message.ID)
	trace(ctx, "sticker enrichment started")
	if err := relay.vk.enrichStickers(ctx, message); err != nil {
		traceFailure(ctx, "sticker enrichment", err)
		return RenderedMessage{}, false, err
	}
	traceMessage(ctx, "enrichment complete", message)
	rendered, err := normalizeMessage(message, "")
	if err != nil {
		traceFailure(ctx, "normalization", err)
		return RenderedMessage{}, false, err
	}
	types := make([]string, 0, len(rendered.Media))
	for _, media := range rendered.Media {
		types = append(types, safeAttachmentType(media.Kind))
	}
	trace(ctx, "message normalized", "text_utf16_units", utf16Length(rendered.Text), "media_types", types, "media_count", len(types), "reply_vk_id", rendered.VKReplyToID)
	if strings.TrimSpace(rendered.Text) == "" && len(rendered.Media) == 0 {
		trace(ctx, "message skipped", "reason", "empty_normalized_content")
		return RenderedMessage{}, false, nil
	}
	if relay.senderNames == nil {
		relay.senderNames = make(map[int64]string)
	}
	name := relay.senderNames[message.FromID]
	if name == "" {
		name = "VK community"
		if message.FromID > 0 {
			trace(ctx, "sender lookup started")
			users, err := relay.vk.UsersGet(ctx, []int64{message.FromID})
			if err != nil {
				traceFailure(ctx, "sender lookup", err)
				return RenderedMessage{}, false, err
			}
			if len(users) != 1 || users[0].ID != message.FromID {
				return RenderedMessage{}, false, ErrVKInvalidResponse
			}
			name = strings.TrimSpace(users[0].FirstName + " " + users[0].LastName)
			if name == "" {
				name = "VK sender"
			}
		}
		if len(relay.senderNames) >= 1024 {
			clear(relay.senderNames)
		}
		relay.senderNames[message.FromID] = name
	}
	rendered.Name = name
	trace(ctx, "message prepared", "accepted", true)
	return rendered, true, nil
}

func retryableVK(err error) bool {
	var apiError *VKAPIError
	if errors.As(err, &apiError) {
		return apiError.Code == 6 || apiError.Code == 9 || apiError.Code == 10 || apiError.Code == 29
	}
	var httpError *VKHTTPError
	if errors.As(err, &httpError) {
		return httpError.StatusCode == 429 || httpError.StatusCode >= 500
	}
	var transportError *VKTransportError
	return errors.As(err, &transportError) && !errors.Is(err, context.Canceled)
}

func (relay *Relay) retry(ctx context.Context) error {
	relay.logger.Warn("transient vk failure; retrying")
	return pause(ctx, relay.retryDelay)
}
