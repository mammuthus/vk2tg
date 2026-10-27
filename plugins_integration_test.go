package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/mammuthus/vk2tg/internal/pluginruntime"
)

type recordingObserver struct {
	events []pluginruntime.Event
	store  *MessageStore
	t      *testing.T
}

func (observer *recordingObserver) Enqueue(ctx context.Context, event pluginruntime.Event) error {
	if observer.store != nil {
		mapped, err := observer.store.Lookup(ctx, event.VKMessageID)
		if err != nil || mapped != event.TelegramMessageID {
			observer.t.Error("plugin called before mapping commit")
		}
	}
	observer.events = append(observer.events, event)
	return errors.New("private plugin error") // Must not become a Telegram failure.
}
func TestPluginEventMetadata(t *testing.T) {
	message, _, err := decodeLongPollMessage([]byte(`[4,42,2,2000000001,123,"text",{"from":"9"},{}]`))
	if err != nil {
		t.Fatal(err)
	}
	rendered, err := normalizeMessage(message, "Sender")
	if err != nil {
		t.Fatal(err)
	}
	event := rendered.PluginEvent
	if event.VKMessageID != 42 || event.VKPeerID != 2000000001 || event.VKSenderID != 9 || !event.Out || event.Text != "text" || !event.PublishedAt.Equal(time.Unix(123, 0).UTC()) {
		t.Fatal("source metadata not preserved")
	}
	for _, raw := range []string{`[4,42,0,2000000001,null,"text",{"from":"9"},{}]`, `[4,42,0,2000000001,"bad","text",{"from":"9"},{}]`} {
		message, _, err = decodeLongPollMessage([]byte(raw))
		if err != nil || message.Date != 0 {
			t.Fatal("optional timestamp changed message acceptance")
		}
	}
}

func TestPluginConfigurationValidation(t *testing.T) {
	env := map[string]string{"VK_ACCESS_TOKEN": "fake", "VK_TARGET_PEER_ID": "1", "TELEGRAM_BOT_TOKEN": "fake", "TELEGRAM_TARGET_CHAT_ID": "-1"}
	config, err := loadConfig(func(key string) string { return env[key] })
	if err != nil || len(config.Plugins) != 0 || config.PluginTimeout != 10*time.Second {
		t.Fatal("plugins must default to disabled")
	}
	env["PLUGINS"] = "not-built-in"
	if _, err := loadConfig(func(key string) string { return env[key] }); err == nil {
		t.Fatal("unknown plugin accepted")
	}
	env["PLUGINS"] = ""
	for _, value := range []string{"0", "-1", "61", "private-value"} {
		env["PLUGIN_TIMEOUT_SECONDS"] = value
		if _, err := loadConfig(func(key string) string { return env[key] }); err == nil {
			t.Fatal("invalid timeout accepted")
		}
	}
}

type forbiddenHistoryObserver struct{ t *testing.T }

func (observer forbiddenHistoryObserver) Enqueue(context.Context, pluginruntime.Event) error {
	observer.t.Error("history mode invoked a plugin")
	return nil
}

// A real worker retry must not re-enter the Telegram delivery path.
type retryTestHandler struct{ calls chan struct{} }

func (h *retryTestHandler) Handle(context.Context, pluginruntime.Event) error {
	h.calls <- struct{}{}
	return errors.New("private error")
}

func TestPluginRetriesNeverResendTelegram(t *testing.T) {
	calls := make(chan struct{}, 10)
	registry := pluginruntime.NewRegistry()
	registry.RegisterDurable("fixture", func() pluginruntime.Handler { return &retryTestHandler{calls: calls} })
	manager, err := pluginruntime.Start(t.Context(), pluginruntime.Options{Names: []string{"fixture"}, Registry: registry, QueuePath: filepath.Join(t.TempDir(), "queue.sqlite"), Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := manager.Close(ctx); err != nil {
			t.Error(err)
		}
	}()
	sends := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sends++
		writeFixture(t, w, `{"ok":true,"result":{"message_id":1001}}`)
	}))
	defer server.Close()
	config := Config{TelegramBotToken: "fake", TelegramTargetChatID: -123}
	client, err := newTestTelegramClient(config, server.URL, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	relay := Relay{config: config, telegram: client, store: testMessageStore(t), plugins: manager, pluginsLive: true, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	message, err := normalizeMessage(VKMessage{ID: 42, PeerID: 2000000001, FromID: 9, Text: "text", Date: 123}, "Sender")
	if err != nil {
		t.Fatal(err)
	}
	if err := relay.deliverMapped(t.Context(), message); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		select {
		case <-calls:
		case <-time.After(3 * time.Second):
			t.Fatal("plugin retry did not happen")
		}
	}
	if err := relay.deliverMapped(t.Context(), message); err != nil {
		t.Fatal(err)
	}
	if sends != 1 {
		t.Fatal("plugin failure repeated Telegram delivery")
	}
	// Even a closed/unavailable queue cannot undo an already successful mapping.
	closeCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	manager.Close(closeCtx)
	message.VKMessageID++
	message.PluginEvent.VKMessageID++
	if err := relay.deliverMapped(t.Context(), message); err != nil {
		t.Fatal("queue error escaped to relay", err)
	}
	if err := relay.deliverMapped(t.Context(), message); err != nil {
		t.Fatal(err)
	}
	if sends != 2 {
		t.Fatal("failed enqueue retried Telegram")
	}
}

func TestSourceConversationMessageIDIsPreserved(t *testing.T) {
	message := VKMessage{}
	if err := json.Unmarshal([]byte(`{"id":42,"peer_id":2000000001,"from_id":9,"text":"#домашка","conversation_message_id":314}`), &message); err != nil {
		t.Fatal(err)
	}
	rendered, err := normalizeMessage(message, "")
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(rendered.PluginEvent)
	if err != nil {
		t.Fatal(err)
	}
	var event map[string]any
	if err := json.Unmarshal(payload, &event); err != nil {
		t.Fatal(err)
	}
	if event["vk_conversation_message_id"] != float64(314) {
		t.Fatal("source conversation message ID missing from plugin event")
	}
}
