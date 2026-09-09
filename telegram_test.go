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

func writeTelegramSuccess(t *testing.T, writer http.ResponseWriter, request *http.Request) {
	t.Helper()
	type sent struct {
		ID int64 `json:"message_id"`
	}
	var result any = sent{ID: 101}
	if strings.HasSuffix(request.URL.Path, "/sendMediaGroup") {
		var items []json.RawMessage
		if err := json.Unmarshal([]byte(request.FormValue("media")), &items); err != nil {
			t.Error(err)
			return
		}
		messages := make([]sent, len(items))
		for index := range messages {
			messages[index].ID = 101 + int64(index)
		}
		result = messages
	}
	if err := json.NewEncoder(writer).Encode(struct {
		OK     bool `json:"ok"`
		Result any  `json:"result"`
	}{true, result}); err != nil {
		t.Error(err)
	}
}

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
	client, err := newTestTelegramClient(Config{TelegramBotToken: "fake-bot-token", TelegramTargetChatID: -123}, server.URL, time.Second*5)
	if err != nil {
		t.Fatal(err)
	}
	started := client.pacer.now()
	if _, err := client.SendMessage(t.Context(), "hello", 0); err != nil {
		t.Fatal(err)
	}
	if calls != 2 || client.pacer.now().Sub(started) < time.Second {
		t.Fatal("429 was not delayed and retried")
	}
}

func TestTelegramReplyAndMessageID(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var payload struct {
			Reply *TelegramReplyParameters `json:"reply_parameters"`
		}
		if json.NewDecoder(request.Body).Decode(&payload) != nil || payload.Reply == nil || payload.Reply.MessageID != 321 || !payload.Reply.AllowSendingWithoutReply {
			t.Error("missing Telegram reply parameters")
		}
		writeFixture(t, writer, `{"ok":true,"result":{"message_id":654}}`)
	}))
	defer server.Close()
	client, err := newTestTelegramClient(Config{TelegramBotToken: "fake-token", TelegramTargetChatID: -123}, server.URL, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	identifier, err := client.SendMessage(t.Context(), "reply", 321)
	if err != nil || identifier != 654 {
		t.Fatalf("incorrect Telegram message ID: %d %v", identifier, err)
	}
}

func TestTelegramRejectsMissingIdentifiers(t *testing.T) {
	for _, result := range []string{`null`, `{}`, `{"message_id":0}`, `{"message_id":-1}`, `{"message_id":"private-value"}`, `[]`} {
		t.Run(result, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				writeFixture(t, writer, `{"ok":true,"result":`+result+`}`)
			}))
			defer server.Close()
			client, err := newTestTelegramClient(Config{TelegramBotToken: "fake-token", TelegramTargetChatID: -123}, server.URL, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			identifier, err := client.SendMessage(t.Context(), "test", 0)
			if err == nil || identifier != 0 || strings.Contains(err.Error(), "private-value") {
				t.Fatal("invalid successful response accepted or leaked")
			}
		})
	}
}

func TestTelegramSafeTransportAndJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writeFixture(t, writer, "invalid fake-bot-token JSON")
	}))
	client, err := newTestTelegramClient(Config{TelegramBotToken: "fake-bot-token", TelegramTargetChatID: -123}, server.URL, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.SendMessage(t.Context(), "hello", 0)
	if err == nil || strings.Contains(err.Error(), "fake-bot-token") {
		t.Fatal("unsafe JSON error")
	}
	server.Close()
	_, err = client.SendMessage(t.Context(), "hello", 0)
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
	client, err := newTestTelegramClient(Config{TelegramBotToken: "fake-bot-token", TelegramTargetChatID: -123}, server.URL, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.SendMessage(ctx, "hello", 0); !errors.Is(err, context.Canceled) {
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
			client, err := newTestTelegramClient(Config{TelegramBotToken: "fake-bot-token", TelegramTargetChatID: -123}, server.URL, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.SendMessage(t.Context(), "hello", 0)
			var apiError *TelegramError
			if !errors.As(err, &apiError) || apiError.HTTPStatus != status || strings.Contains(err.Error(), "fake-bot-token") {
				t.Fatalf("unsafe or missing error: %v", err)
			}
		})
	}
}
