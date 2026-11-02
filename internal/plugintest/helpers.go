package plugintest

import (
	"bytes"
	"context"
	"sync"
	"testing"
	"time"

	"github.com/mammuthus/vk2tg/internal/pluginruntime"
)

type Handler struct {
	HandleFunc func(context.Context, pluginruntime.Event) error
	InitFunc   func(context.Context, pluginruntime.Settings) error
	CloseFunc  func(context.Context) error
}

func (handler *Handler) Handle(ctx context.Context, event pluginruntime.Event) error {
	if handler.HandleFunc != nil {
		return handler.HandleFunc(ctx, event)
	}
	return nil
}
func (handler *Handler) Init(ctx context.Context, settings pluginruntime.Settings) error {
	if handler.InitFunc != nil {
		return handler.InitFunc(ctx, settings)
	}
	return nil
}
func (handler *Handler) Close(ctx context.Context) error {
	if handler.CloseFunc != nil {
		return handler.CloseFunc(ctx)
	}
	return nil
}
func FixtureEvent() pluginruntime.Event {
	return pluginruntime.Event{VKMessageID: 42, VKPeerID: 2000000001, VKSenderID: 9, TelegramChatID: -123, TelegramMessageID: 1001, Text: "private text", PublishedAt: time.Unix(123, 0).UTC(), Attachments: []pluginruntime.Attachment{{Kind: "photo", URL: "https://example.invalid/private", Name: "private.jpg"}}}
}
func CloseFixture(t *testing.T, manager *pluginruntime.Manager) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := manager.Close(ctx); err != nil {
		t.Fatal(err)
	}
}
func Eventually(t *testing.T, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not satisfied")
}

// Reads may race with a worker writing a diagnostic record.
type LogBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (buffer *LogBuffer) Write(data []byte) (int, error) {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.buffer.Write(data)
}
func (buffer *LogBuffer) String() string {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.buffer.String()
}
func ReceiveEvent(t *testing.T, events <-chan pluginruntime.Event) pluginruntime.Event {
	t.Helper()
	select {
	case e := <-events:
		return e
	case <-time.After(3 * time.Second):
		t.Fatal("observer did not receive event")
		return pluginruntime.Event{}
	}
}
