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
	config      Config
	vk          *VKClient
	telegram    *TelegramClient
	mediaHTTP   *http.Client
	logger      *slog.Logger
	tempRoot    string
	retryDelay  time.Duration
	senderNames map[int64]string
}

func (relay *Relay) Run(ctx context.Context) error {
	if relay.retryDelay <= 0 {
		relay.retryDelay = 2 * time.Second
	}
	relay.senderNames = make(map[int64]string)
	var server VKLongPollServer
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
		}
		batch, err := relay.vk.WaitLongPoll(ctx, server)
		if err != nil {
			if !retryableVK(err) && !errors.Is(err, ErrVKInvalidJSON) && !errors.Is(err, ErrVKInvalidResponse) {
				return err
			}
			if err := relay.retry(ctx); err != nil {
				return err
			}
			continue
		}
		switch batch.Failed {
		case 0:
		case 1:
			server.TS = batch.TS
			continue
		case 2:
			server.Key = ""
			continue
		case 3:
			relay.logger.Warn("long poll cursor expired; resuming from current events")
			server = VKLongPollServer{}
			continue
		default:
			return errors.New("unsupported vk long poll protocol response")
		}
		for _, event := range batch.Updates {
			var message RenderedMessage
			var accepted bool
			for {
				message, accepted, err = relay.prepare(ctx, event)
				if err == nil {
					break
				}
				if !retryableVK(err) {
					return err
				}
				if err := relay.retry(ctx); err != nil {
					return err
				}
			}
			if !accepted {
				continue
			}
			if relay.config.DryRun {
				relay.logger.Info("would relay message", "media_count", len(message.Media), "repost", message.Repost)
				continue
			}
			if err := deliverMessage(ctx, relay.telegram, relay.mediaHTTP, relay.tempRoot, message); err != nil {
				return err
			}
			relay.logger.Info("message relayed", "media_count", len(message.Media), "repost", message.Repost)
		}
		server.TS = batch.TS
	}
}

func (relay *Relay) accepts(message VKMessage) bool {
	if message.ID == 0 || message.PeerID != relay.config.VKTargetPeerID || message.Out != 0 || message.FromID == 0 {
		return false
	}
	_, blocked := relay.config.VKBlockedSenderIDs[message.FromID]
	return !blocked
}

func (relay *Relay) prepare(ctx context.Context, event json.RawMessage) (RenderedMessage, bool, error) {
	message, needsFull, err := decodeLongPollMessage(event)
	if err != nil {
		return RenderedMessage{}, false, err
	}
	if !relay.accepts(message) {
		return RenderedMessage{}, false, nil
	}
	if needsFull {
		message, err = relay.vk.GetMessageByID(ctx, message.ID)
		if err != nil {
			return RenderedMessage{}, false, err
		}
		if !relay.accepts(message) {
			return RenderedMessage{}, false, nil
		}
	}
	name := relay.senderNames[message.FromID]
	if name == "" {
		name = "VK community"
		if message.FromID > 0 {
			users, err := relay.vk.UsersGet(ctx, []int64{message.FromID})
			if err != nil {
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
	rendered, err := normalizeMessage(message, name)
	return rendered, err == nil, err
}

func retryableVK(err error) bool {
	var apiError *VKAPIError
	if errors.As(err, &apiError) {
		return apiError.Code == 6 || apiError.Code == 9 || apiError.Code == 10
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
