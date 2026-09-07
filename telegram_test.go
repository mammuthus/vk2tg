package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestTelegramSendMessage(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls++
		if request.URL.Path != "/botfake-bot-token/sendMessage" || request.Method != http.MethodPost {
			t.Error("unexpected Telegram request")
		}
		var payload map[string]any
		if json.NewDecoder(request.Body).Decode(&payload) != nil {
			t.Error("invalid request JSON")
		}
		if payload["text"] != "hello" || payload["parse_mode"] != "HTML" || payload["chat_id"] != float64(-123) {
			t.Error("incorrect sendMessage payload")
		}
		if calls == 1 {
			writer.WriteHeader(429)
			writeFixture(t, writer, `{"ok":false,"error_code":429,"description":"fake-bot-token","parameters":{"retry_after":1}}`)
			return
		}
		writeFixture(t, writer, `{"ok":true,"result":{"message_id":1}}`)
	}))
	defer server.Close()
	client, err := NewTelegramClient(Config{TelegramBotToken: "fake-bot-token", TelegramTargetChatID: -123}, server.URL, time.Second*5)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	if err := client.SendMessage(t.Context(), "hello"); err != nil {
		t.Fatal(err)
	}
	if calls != 2 || time.Since(started) < time.Second {
		t.Fatal("429 was not delayed and retried")
	}
}

func TestTelegramSafeTransportAndJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writeFixture(t, writer, "invalid fake-bot-token JSON")
	}))
	client, err := NewTelegramClient(Config{TelegramBotToken: "fake-bot-token", TelegramTargetChatID: -123}, server.URL, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	err = client.SendMessage(t.Context(), "hello")
	if err == nil || strings.Contains(err.Error(), "fake-bot-token") {
		t.Fatal("unsafe JSON error")
	}
	server.Close()
	err = client.SendMessage(t.Context(), "hello")
	if err == nil || strings.Contains(err.Error(), "fake-bot-token") || strings.Contains(err.Error(), server.URL) {
		t.Fatal("unsafe transport error")
	}
}

func TestTelegramRateLimitCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls++
		writer.WriteHeader(429)
		writeFixture(t, writer, `{"ok":false,"error_code":429,"parameters":{"retry_after":3600}}`)
		cancel()
	}))
	defer server.Close()
	client, err := NewTelegramClient(Config{TelegramBotToken: "fake-bot-token", TelegramTargetChatID: -123}, server.URL, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.SendMessage(ctx, "hello"); !errors.Is(err, context.Canceled) {
		t.Fatalf("rate-limit cancellation failed: %v", err)
	}
	if calls != 1 {
		t.Fatal("canceled send was retried")
	}
}

func TestTelegramErrors(t *testing.T) {
	for _, status := range []int{200, 400, 500} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				writer.WriteHeader(status)
				writeFixture(t, writer, `{"ok":false,"error_code":400,"description":"fake-bot-token"}`)
			}))
			defer server.Close()
			client, err := NewTelegramClient(Config{TelegramBotToken: "fake-bot-token", TelegramTargetChatID: -123}, server.URL, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			err = client.SendMessage(t.Context(), "hello")
			var apiError *TelegramError
			if !errors.As(err, &apiError) || apiError.HTTPStatus != status || strings.Contains(err.Error(), "fake-bot-token") {
				t.Fatalf("unsafe or missing error: %v", err)
			}
		})
	}
}
