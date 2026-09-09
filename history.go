package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"net/url"
	"strconv"
	"strings"
	"time"
)

func parseHistoryCommand(args []string) (int, error) {
	if len(args) == 0 {
		return 0, nil
	}
	if args[0] != "test-history" {
		return 0, errors.New("unknown command; use test-history --count N")
	}
	flags := flag.NewFlagSet("test-history", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	count := flags.Int("count", 3, "number of latest messages")
	if flags.Parse(args[1:]) != nil || flags.NArg() != 0 {
		return 0, errors.New("invalid history arguments; use test-history --count N")
	}
	if *count < 1 || *count > 100 {
		return 0, errors.New("history count must be between 1 and 100")
	}
	return *count, nil
}

func (client *VKClient) GetHistory(ctx context.Context, peerID int64, count int) ([]VKMessage, error) {
	if count < 1 || count > 100 {
		return nil, errors.New("history count must be between 1 and 100")
	}
	var response struct {
		Items []VKMessage `json:"items"`
	}
	parameters := url.Values{"peer_id": {strconv.FormatInt(peerID, 10)}, "count": {strconv.Itoa(count)}, "offset": {"0"}, "rev": {"0"}}
	if err := client.call(ctx, "messages.getHistory", parameters, &response); err != nil {
		return nil, err
	}
	if response.Items == nil || len(response.Items) > count {
		return nil, ErrVKInvalidResponse
	}
	seen := make(map[int64]struct{})
	for _, message := range response.Items {
		if message.ID == 0 || message.FromID == 0 || message.PeerID != peerID {
			return nil, ErrVKInvalidResponse
		}
		if _, exists := seen[message.ID]; exists {
			return nil, ErrVKInvalidResponse
		}
		seen[message.ID] = struct{}{}
	}
	for left, right := 0, len(response.Items)-1; left < right; left, right = left+1, right-1 {
		response.Items[left], response.Items[right] = response.Items[right], response.Items[left]
	}
	return response.Items, nil
}

func (relay *Relay) ReplayHistory(ctx context.Context, count int) error {
	if relay.retryDelay <= 0 {
		relay.retryDelay = 2 * time.Second
	}
	var messages []VKMessage
	for {
		var err error
		messages, err = relay.vk.GetHistory(ctx, relay.config.VKTargetPeerID, count)
		if err == nil {
			break
		}
		var apiError *VKAPIError
		if errors.As(err, &apiError) && (apiError.Code == 9 || apiError.Code == 29) {
			return err
		}
		if !retryableVK(err) {
			return err
		}
		if err := relay.retry(ctx); err != nil {
			return err
		}
	}
	relay.logger.Info("history selected", "selected", len(messages), "requested", count, "dry_run", relay.config.DryRun)
	sent, skipped := 0, 0
	replayMap := make(map[int64]int64)
	for index, message := range messages {
		attachmentTypes := make([]string, 0, len(message.Attachments))
		for _, attachment := range message.Attachments {
			attachmentTypes = append(attachmentTypes, attachment.Type)
		}
		actionType := ""
		if message.Action != nil {
			actionType = message.Action.Type
		}
		_, blocked := relay.config.VKBlockedSenderIDs[message.FromID]
		relay.logger.Info("history message inspected", "position", index+1, "message_id", message.ID, "sender_id", message.FromID, "peer_id", message.PeerID, "outbox", message.Out != 0, "text_nonempty", strings.TrimSpace(message.Text) != "", "action_type", actionType, "has_reply", message.ReplyMessage != nil, "attachment_types", attachmentTypes, "blocked_sender", blocked)
		candidate := message
		candidate.Out = 0
		if !relay.accepts(candidate) {
			skipped++
			relay.logger.Info("history message skipped", "position", index+1, "message_id", message.ID, "reason", relay.historySkipReason(candidate))
			continue
		}
		rendered, accepted, err := relay.prepareMessage(ctx, message)
		if err != nil {
			return err
		}
		if !accepted {
			skipped++
			reason := "empty_content"
			if message.Action != nil && strings.TrimSpace(message.Text) == "" && len(message.Attachments) == 0 {
				reason = "empty_service_action"
			}
			relay.logger.Info("history message skipped", "position", index+1, "message_id", message.ID, "reason", reason)
			continue
		}
		photos, documents := 0, 0
		for _, media := range rendered.Media {
			switch media.Kind {
			case "photo":
				photos++
			case "document", "sticker":
				documents++
			}
		}
		if relay.config.DryRun {
			relay.logger.Info("would replay history message", "position", index+1, "repost", rendered.Repost, "photos", photos, "documents", documents, "unsupported_attachments", rendered.UnsupportedAttachments, "wall_fallbacks", rendered.WallFallbacks, "ignored_forwards", rendered.IgnoredForwards, "vk_reply", rendered.VKReplyToID > 0)
			continue
		}
		if rendered.VKReplyToID > 0 {
			rendered.TelegramReplyID = replayMap[rendered.VKReplyToID]
			if rendered.TelegramReplyID == 0 && relay.store != nil {
				rendered.TelegramReplyID, err = relay.store.Lookup(ctx, rendered.VKReplyToID)
				if err != nil {
					return err
				}
			}
		}
		canonical, err := deliverMessage(ctx, relay.telegram, relay.mediaHTTP, relay.tempRoot, rendered)
		if err != nil {
			return err
		}
		replayMap[message.ID] = canonical
		sent++
		relay.logger.Info("history message sent", "position", index+1, "repost", rendered.Repost, "photos", photos, "documents", documents, "unsupported_attachments", rendered.UnsupportedAttachments, "wall_fallbacks", rendered.WallFallbacks, "ignored_forwards", rendered.IgnoredForwards, "vk_reply", rendered.VKReplyToID > 0, "telegram_reply", rendered.TelegramReplyID > 0)
	}
	relay.logger.Info("history replay complete", "selected", len(messages), "sent", sent, "skipped", skipped, "dry_run", relay.config.DryRun)
	return nil
}

func (relay *Relay) historySkipReason(message VKMessage) string {
	if message.PeerID != relay.config.VKTargetPeerID {
		return "wrong_peer"
	}
	if message.FromID == 0 {
		return "missing_sender"
	}
	if _, blocked := relay.config.VKBlockedSenderIDs[message.FromID]; blocked {
		return "blocked_sender"
	}
	return "not_accepted"
}
