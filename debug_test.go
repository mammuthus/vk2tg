package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestDebugTrace(t *testing.T) {
	for _, testCase := range []struct {
		name, reason                                           string
		flags, peer, sender                                    int
		photo, telegramFailure, mediaFailure, rateLimit, quiet bool
	}{
		{name: "accepted"},
		{name: "outbox", flags: 2, photo: true, reason: "outbox"},
		{name: "wrong peer", peer: 2000000002, photo: true, reason: "wrong_peer"},
		{name: "blocklist", sender: 73, photo: true, reason: "blocked_sender"},
		{name: "media", photo: true},
		{name: "Telegram failure", photo: true, telegramFailure: true},
		{name: "media failure", photo: true, mediaFailure: true},
		{name: "Telegram retry", photo: true, rateLimit: true},
		{name: "debug disabled", quiet: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			var output bytes.Buffer
			polls, sends, fullCalls, downloads := 0, 0, 0, 0
			var server *httptest.Server
			server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				switch request.URL.Path {
				case "/messages.getLongPollServer":
					writeFixture(t, writer, fmt.Sprintf(`{"response":{"server":%q,"key":"secret-poll-key","ts":"10"}}`, server.URL+"/poll"))
				case "/poll":
					polls++
					if polls > 1 {
						writeFixture(t, writer, `{"failed":4}`)
						return
					}
					peer, sender := testCase.peer, testCase.sender
					if peer == 0 {
						peer = 2000000001
					}
					if sender == 0 {
						sender = 42
					}
					hints, extra := `{}`, `{"from":"42"}`
					extra = fmt.Sprintf(`{"from":"%d"}`, sender)
					if testCase.photo {
						hints = `{"attach1_type":"photo","attach1":"private-media-url"}`
						extra = fmt.Sprintf(`{"from":"%d","reply":"private-reply-content"}`, sender)
					}
					writeFixture(t, writer, fmt.Sprintf(`{"ts":"11","updates":[[4,99,%d,%d,123,"private-message-text",%s,%s]]}`, testCase.flags, peer, extra, hints))
				case "/messages.getById":
					fullCalls++
					writeFixture(t, writer, fmt.Sprintf(`{"response":{"items":[{"id":99,"peer_id":2000000001,"from_id":42,"text":"private-full-text","reply_message":{"id":98},"attachments":[{"type":"photo","photo":{"sizes":[{"url":%q,"width":100,"height":100}]}}]}]}}`, server.URL+"/private-media-url"))
				case "/users.get":
					writeFixture(t, writer, `{"response":[{"id":42,"first_name":"PrivateName"}]}`)
				case "/private-media-url":
					downloads++
					if testCase.mediaFailure {
						writer.WriteHeader(403)
						return
					}
					writeFixture(t, writer, "private-binary-payload")
				case "/botsecret-telegram-token/sendMessage", "/botsecret-telegram-token/sendPhoto":
					sends++
					if testCase.telegramFailure {
						writer.WriteHeader(400)
						writeFixture(t, writer, `{"ok":false,"error_code":400,"description":"private-error-description"}`)
						return
					}
					if testCase.rateLimit && sends == 1 {
						writer.WriteHeader(429)
						writeFixture(t, writer, `{"ok":false,"error_code":429,"parameters":{"retry_after":1}}`)
						return
					}
					writeFixture(t, writer, `{"ok":true,"result":{"message_id":501,"chat":{"type":"channel"}}}`)
				default:
					t.Error("unexpected request")
					writer.WriteHeader(400)
				}
			}))
			defer server.Close()
			config := Config{Debug: !testCase.quiet, VKAccessToken: "secret-vk-token", VKTargetPeerID: 2000000001, VKBlockedSenderIDs: map[int64]struct{}{73: {}}, TelegramBotToken: "secret-telegram-token", TelegramTargetChatID: -123}
			vk, err := NewVKClient(config, server.URL, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			telegram, err := newTestTelegramClient(config, server.URL, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			store := testMessageStore(t)
			relay := Relay{config: config, vk: vk, telegram: telegram, store: store, mediaHTTP: server.Client(), tempRoot: t.TempDir(), logger: slog.New(slog.NewJSONHandler(&output, &slog.HandlerOptions{Level: slog.LevelDebug}))}
			if relay.Run(t.Context()) == nil {
				t.Fatal("expected final protocol or delivery error")
			}
			for _, secret := range []string{"secret-vk-token", "secret-telegram-token", "secret-poll-key", "private-", "PrivateName", server.URL} {
				if strings.Contains(output.String(), secret) {
					t.Fatalf("debug log leaked %q", secret)
				}
			}
			events := make(map[string][]map[string]any)
			for _, line := range strings.Split(strings.TrimSpace(output.String()), "\n") {
				if strings.Count(line, `"vk_message_id":`) > 1 {
					t.Fatal("duplicate correlation key")
				}
				var row map[string]any
				if err := json.Unmarshal([]byte(line), &row); err != nil {
					t.Fatal(err)
				}
				if row["level"] == "DEBUG" {
					events[row["msg"].(string)] = append(events[row["msg"].(string)], row)
				}
			}
			if testCase.quiet {
				if len(events) != 0 {
					t.Fatal("debug events emitted while disabled")
				}
				return
			}
			requireEvent := func(name string) map[string]any {
				t.Helper()
				if len(events[name]) == 0 {
					t.Fatalf("missing event %q", name)
				}
				return events[name][0]
			}
			if row := requireEvent("message event received"); row["vk_message_id"] != float64(99) || row["ts_before"] != "10" || row["ts_after"] != "11" {
				t.Fatal("missing event correlation")
			}
			mapping, err := store.Lookup(t.Context(), 99)
			if err != nil {
				t.Fatal(err)
			}
			if testCase.reason != "" {
				if requireEvent("message skipped")["reason"] != testCase.reason {
					t.Fatal("incorrect skip reason")
				}
				if sends != 0 || fullCalls != 0 || downloads != 0 || mapping != 0 {
					t.Fatal("filtered event performed downstream work")
				}
				requireEvent("long poll ts advanced")
				return
			}
			requireEvent("message filters accepted")
			requireEvent("message normalized")
			if testCase.photo {
				requireEvent("getById succeeded")
				requireEvent("reply mapping resolved")
				if requireEvent("media download started")["vk_message_id"] != float64(99) {
					t.Fatal("media lost correlation")
				}
			}
			if testCase.mediaFailure {
				requireEvent("media download failed")
				if sends != 0 || mapping != 0 || len(events["long poll ts advanced"]) != 0 {
					t.Fatal("failed media advanced or sent")
				}
				return
			}
			if testCase.photo {
				requireEvent("media download succeeded")
			}
			method := "sendMessage"
			if testCase.photo {
				method = "sendPhoto"
			}
			if requireEvent("telegram method selected")["telegram_method"] != method {
				t.Fatal("wrong Telegram method")
			}
			if testCase.telegramFailure {
				if requireEvent("telegram request failed")["api_code"] != float64(400) {
					t.Fatal("missing safe API error")
				}
				if mapping != 0 || len(events["long poll ts advanced"]) != 0 {
					t.Fatal("failed send saved or advanced")
				}
				return
			}
			if testCase.rateLimit {
				requireEvent("telegram retry scheduled")
				requireEvent("telegram pacing wait")
				if sends != 2 {
					t.Fatal("429 did not retry")
				}
			}
			for _, name := range []string{"telegram request succeeded", "canonical telegram message", "mapping save succeeded"} {
				row := requireEvent(name)
				if row["vk_message_id"] != float64(99) || row["telegram_message_id"] != float64(501) {
					t.Fatalf("lost mapping correlation: %s", name)
				}
			}
			if mapping != 501 {
				t.Fatal("mapping not saved")
			}
			requireEvent("long poll batch complete")
			requireEvent("long poll ts advanced")
		})
	}
}

func TestDebugParsingAndEnrichmentFilters(t *testing.T) {
	for _, testCase := range []struct {
		name, event, reason string
		duplicate           bool
		wantFull            int
	}{
		{name: "empty service", event: `[4,99,0,2000000001,123,"",{"from":"42","source_act":"private-action"},{}]`, reason: "empty_service_event"},
		{name: "enriched outbox", event: `[4,99,0,2000000001,123,"private-text",{"from":"42","reply":"private-reply"},{}]`, reason: "outbox", wantFull: 1},
		{name: "duplicate", event: `[4,99,0,2000000001,123,"private-text",{"from":"42"},{"attach1_type":"photo"}]`, reason: "duplicate_mapping", duplicate: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			var output bytes.Buffer
			fullCalls := 0
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.URL.Path != "/messages.getById" {
					t.Error("unexpected request")
					writer.WriteHeader(400)
					return
				}
				fullCalls++
				writeFixture(t, writer, `{"response":{"items":[{"id":99,"peer_id":2000000001,"from_id":42,"out":1,"text":"private-text","reply_message":{"id":98}}]}}`)
			}))
			defer server.Close()
			config := Config{Debug: true, VKAccessToken: "private-token", VKTargetPeerID: 2000000001}
			vk, err := NewVKClient(config, server.URL, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			store := testMessageStore(t)
			if testCase.duplicate {
				if err := store.Save(t.Context(), 99, 501); err != nil {
					t.Fatal(err)
				}
			}
			relay := Relay{config: config, vk: vk, store: store, logger: slog.New(slog.NewJSONHandler(&output, &slog.HandlerOptions{Level: slog.LevelDebug}))}
			_, accepted, err := relay.prepare(t.Context(), json.RawMessage(testCase.event))
			if err != nil || accepted || fullCalls != testCase.wantFull {
				t.Fatal("skip behavior changed")
			}
			if !strings.Contains(output.String(), `"reason":"`+testCase.reason+`"`) {
				t.Fatal("missing skip reason")
			}
			if strings.Contains(output.String(), "private-") {
				t.Fatal("content leak")
			}
			if testCase.wantFull > 0 && !strings.Contains(output.String(), `"reply_vk_id":98`) {
				t.Fatal("missing enriched reply ID")
			}
		})
	}
}

func TestDebugUntrustedMetadata(t *testing.T) {
	var output bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&output, &slog.HandlerOptions{Level: slog.LevelDebug}))
	ctx := debugContext(t.Context(), logger, "vk_message_id", int64(99))
	_, needed, err := decodeLongPollMessageContext(ctx, json.RawMessage(`[4,99,0,2000000001,123,"private-text",{"from":"42"},{"attach1_type":"private-media-url","attach1":"private-token"}]`))
	if err != nil || !needed {
		t.Fatal("unexpected parser result")
	}
	traceMessage(ctx, "metadata", VKMessage{Text: "private-text", Attachments: []VKAttachment{{Type: "private-media-url"}}})
	traceFailure(ctx, "request", fmt.Errorf("private-raw-error"))
	trace(ctx, "cursor", "ts", safeTS(json.Number("private-key")))
	if strings.Contains(output.String(), "private-") || !strings.Contains(output.String(), `"unknown"`) {
		t.Fatal("untrusted metadata was not sanitized")
	}
}
