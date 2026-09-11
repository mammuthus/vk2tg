package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sort"
	"strings"
)

type debugLoggerKey struct{}
type debugMessageKey struct{}

func debugContext(ctx context.Context, logger *slog.Logger, attrs ...any) context.Context {
	if parent, ok := ctx.Value(debugLoggerKey{}).(*slog.Logger); ok {
		logger = parent
	}
	if logger == nil || !logger.Enabled(ctx, slog.LevelDebug) {
		return ctx
	}
	filtered := make([]any, 0, len(attrs))
	for index := 0; index+1 < len(attrs); index += 2 {
		if attrs[index] == "vk_message_id" {
			identifier, valid := attrs[index+1].(int64)
			if valid {
				if previous, exists := ctx.Value(debugMessageKey{}).(int64); exists && previous == identifier {
					continue
				}
				ctx = context.WithValue(ctx, debugMessageKey{}, identifier)
			}
		}
		filtered = append(filtered, attrs[index], attrs[index+1])
	}
	return context.WithValue(ctx, debugLoggerKey{}, logger.With(filtered...))
}

func trace(ctx context.Context, event string, attrs ...any) {
	if logger, ok := ctx.Value(debugLoggerKey{}).(*slog.Logger); ok {
		logger.DebugContext(ctx, event, attrs...)
	}
}

func traceFailure(ctx context.Context, stage string, err error, attrs ...any) {
	if err == nil {
		return
	}
	kind := "operation"
	var vkAPI *VKAPIError
	var vkHTTP *VKHTTPError
	var vkTransport *VKTransportError
	var telegram *TelegramError
	switch {
	case errors.Is(err, context.Canceled):
		kind = "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		kind = "deadline"
	case errors.Is(err, ErrVKInvalidJSON):
		kind = "vk_invalid_json"
	case errors.Is(err, ErrVKInvalidResponse):
		kind = "vk_invalid_response"
	case errors.As(err, &vkTransport):
		kind = "vk_transport"
	case errors.As(err, &vkAPI):
		kind = "vk_api"
		attrs = append(attrs, "api_code", vkAPI.Code)
	case errors.As(err, &vkHTTP):
		kind = "vk_http"
		attrs = append(attrs, "http_status", vkHTTP.StatusCode)
	case errors.As(err, &telegram):
		kind = "telegram_api"
		attrs = append(attrs, "api_code", telegram.Code, "http_status", telegram.HTTPStatus)
	}
	trace(ctx, stage+" failed", append(attrs, "error_kind", kind)...)
}

func (relay *Relay) debugContext(ctx context.Context, attrs ...any) context.Context {
	if !relay.config.Debug {
		return ctx
	}
	return debugContext(ctx, relay.logger, attrs...)
}

func eventContext(ctx context.Context, raw json.RawMessage) context.Context {
	if ctx.Value(debugLoggerKey{}) == nil {
		return ctx
	}
	var event []json.RawMessage
	var eventType int
	var identifier int64
	if json.Unmarshal(raw, &event) == nil && len(event) > 1 && json.Unmarshal(event[0], &eventType) == nil && eventType == 4 && json.Unmarshal(event[1], &identifier) == nil {
		return debugContext(ctx, nil, "vk_message_id", identifier)
	}
	return ctx
}

func safeAttachmentType(kind string) string {
	switch kind {
	case "photo", "doc", "document", "sticker", "wall", "link", "video", "audio", "audio_message", "gift", "market", "poll", "graffiti":
		return kind
	default:
		return "unknown"
	}
}

func attachmentHintTypes(hints map[string]json.RawMessage) []string {
	types := make([]string, 0)
	for key, value := range hints {
		if strings.HasPrefix(key, "attach") && strings.HasSuffix(key, "_type") {
			var kind string
			if json.Unmarshal(value, &kind) == nil {
				types = append(types, safeAttachmentType(kind))
			}
		}
	}
	sort.Strings(types)
	return types
}

func traceMessage(ctx context.Context, event string, message VKMessage) {
	if ctx.Value(debugLoggerKey{}) == nil {
		return
	}
	types := make([]string, 0, len(message.Attachments))
	for _, attachment := range message.Attachments {
		types = append(types, safeAttachmentType(attachment.Type))
	}
	var replyID int64
	if message.ReplyMessage != nil {
		replyID = message.ReplyMessage.ID
	}
	trace(ctx, event, "peer_id", message.PeerID, "sender_id", message.FromID, "outbox", message.Out,
		"reply_present", message.ReplyMessage != nil, "reply_vk_id", replyID, "action_present", message.Action != nil,
		"text_utf16_units", utf16Length(message.Text), "attachment_types", types, "attachment_count", len(types))
}

func safeTS(value json.Number) string {
	if _, err := value.Int64(); err != nil {
		return "unavailable"
	}
	return value.String()
}
