package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

func TestRelayPersistentReplies(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mapping.sqlite")
	for _, testCase := range []struct {
		name                          string
		messageID, replyVK, wantReply int64
		duplicate, fail               bool
	}{
		{name: "A", messageID: 1},
		{name: "B replies after restart", messageID: 2, replyVK: 1, wantReply: 101},
		{name: "missing target", messageID: 3, replyVK: 999},
		{name: "duplicate", messageID: 1, duplicate: true},
		{name: "send failure", messageID: 4, fail: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			store, err := OpenMessageStore(t.Context(), path)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			polls, sends, metadata := 0, 0, 0
			var server *httptest.Server
			server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				switch request.URL.Path {
				case "/messages.getLongPollServer":
					writeFixture(t, writer, fmt.Sprintf(`{"response":{"server":%q,"key":"key","ts":"10"}}`, server.URL+"/poll"))
				case "/poll":
					polls++
					if polls > 1 {
						writeFixture(t, writer, `{"failed":4}`)
						return
					}
					extra := `{"from":"42"}`
					if testCase.replyVK != 0 {
						extra = `{"from":"42","reply":"hint"}`
					}
					writeFixture(t, writer, fmt.Sprintf(`{"ts":"11","updates":[[4,%d,0,2000000001,123,"text",%s,{}]]}`, testCase.messageID, extra))
				case "/messages.getById":
					writeFixture(t, writer, fmt.Sprintf(`{"response":{"items":[{"id":%d,"peer_id":2000000001,"from_id":42,"text":"reply text","reply_message":{"id":%d}}]}}`, testCase.messageID, testCase.replyVK))
				case "/users.get":
					metadata++
					writeFixture(t, writer, `{"response":[{"id":42,"first_name":"Sender","last_name":""}]}`)
				case "/botfake-token/sendMessage":
					sends++
					var payload struct {
						Reply *TelegramReplyParameters `json:"reply_parameters"`
					}
					if json.NewDecoder(request.Body).Decode(&payload) != nil {
						t.Error("invalid send payload")
					}
					if testCase.wantReply == 0 {
						if payload.Reply != nil {
							t.Error("unknown target should omit reply")
						}
					} else if payload.Reply == nil || payload.Reply.MessageID != testCase.wantReply {
						t.Error("SQLite target not used as reply")
					}
					if testCase.fail {
						writer.WriteHeader(400)
						writeFixture(t, writer, `{"ok":false,"error_code":400}`)
						return
					}
					writeFixture(t, writer, fmt.Sprintf(`{"ok":true,"result":{"message_id":%d}}`, 100+testCase.messageID))
				default:
					t.Errorf("unexpected or write endpoint: %s", request.URL.Path)
					writer.WriteHeader(400)
				}
			}))
			defer server.Close()
			config := Config{VKAccessToken: "fake-token", VKTargetPeerID: 2000000001, TelegramBotToken: "fake-token", TelegramTargetChatID: -123}
			vk, err := NewVKClient(config, server.URL, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			telegram, err := NewTelegramClient(config, server.URL, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			relay := Relay{config: config, vk: vk, telegram: telegram, store: store, mediaHTTP: server.Client(), logger: slog.New(slog.NewTextHandler(io.Discard, nil)), tempRoot: t.TempDir()}
			if relay.Run(t.Context()) == nil {
				t.Fatal("expected final protocol/send stop")
			}
			if testCase.duplicate {
				if sends != 0 || metadata != 0 {
					t.Fatal("duplicate VK ID was sent or enriched again")
				}
			} else if sends != 1 {
				t.Fatal("incorrect number of sends")
			}
			identifier, err := store.Lookup(t.Context(), testCase.messageID)
			if err != nil {
				t.Fatal(err)
			}
			if testCase.fail {
				if identifier != 0 {
					t.Fatal("failed send created mapping")
				}
			} else if identifier != 100+testCase.messageID {
				t.Fatal("mapping not saved")
			}
		})
	}
}

func TestCanonicalMappingForMedia(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint(fail), func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.URL.Path == "/media" {
					writeFixture(t, writer, "bytes")
					return
				}
				calls++
				if calls == 1 && request.URL.Path != "/botfake-token/sendPhoto" {
					t.Error("first primary message must be photo")
				}
				if calls == 2 && request.URL.Path != "/botfake-token/sendDocument" {
					t.Error("second send must be document")
				}
				if fail && calls == 2 {
					writer.WriteHeader(400)
					writeFixture(t, writer, `{"ok":false,"error_code":400}`)
					return
				}
				writeFixture(t, writer, fmt.Sprintf(`{"ok":true,"result":{"message_id":%d}}`, 500+calls))
			}))
			defer server.Close()
			telegram, err := NewTelegramClient(Config{TelegramBotToken: "fake-token", TelegramTargetChatID: -123}, server.URL, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			store := testMessageStore(t)
			relay := Relay{telegram: telegram, store: store, mediaHTTP: server.Client(), tempRoot: t.TempDir()}
			message := RenderedMessage{VKMessageID: 42, Name: "Sender", Repost: true, Text: "wall", Media: []MediaSource{{Kind: "photo", URL: server.URL + "/media", Name: "photo.jpg"}, {Kind: "document", URL: server.URL + "/media", Name: "doc.txt"}}}
			err = relay.deliverMapped(t.Context(), message)
			if (err != nil) != fail {
				t.Fatalf("unexpected delivery error: %v", err)
			}
			identifier, err := store.Lookup(t.Context(), 42)
			if err != nil {
				t.Fatal(err)
			}
			if fail {
				if identifier != 0 {
					t.Fatal("partial media send created mapping")
				}
			} else {
				if identifier != 501 {
					t.Fatal("canonical mapping is not first photo ID")
				}
				if err := relay.deliverMapped(t.Context(), message); err != nil {
					t.Fatal(err)
				}
				if calls != 2 {
					t.Fatal("duplicate mapped delivery repeated sends")
				}
			}
		})
	}
}

func TestHistoryMappingIsolation(t *testing.T) {
	store := testMessageStore(t)
	if err := store.Save(t.Context(), 1, 9001); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(t.Context(), 99, 9099); err != nil {
		t.Fatal(err)
	}
	sends := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/messages.getHistory":
			writeFixture(t, writer, `{"response":{"items":[{"id":3,"peer_id":2000000001,"from_id":42,"text":"C","reply_message":{"id":99}},{"id":2,"peer_id":2000000001,"from_id":42,"text":"B","reply_message":{"id":1}},{"id":1,"peer_id":2000000001,"from_id":42,"out":1,"text":"A"}]}}`)
		case "/users.get":
			writeFixture(t, writer, `{"response":[{"id":42,"first_name":"Sender","last_name":""}]}`)
		case "/botfake-token/sendMessage":
			sends++
			var payload struct {
				Reply *TelegramReplyParameters `json:"reply_parameters"`
			}
			if json.NewDecoder(request.Body).Decode(&payload) != nil {
				t.Error("invalid JSON")
			}
			switch sends {
			case 1:
				if payload.Reply != nil {
					t.Error("A must not be reply")
				}
			case 2:
				if payload.Reply == nil || payload.Reply.MessageID != 501 {
					t.Error("B must reply to replay A, not normal mapping")
				}
			case 3:
				if payload.Reply == nil || payload.Reply.MessageID != 9099 {
					t.Error("external target must resolve from SQLite")
				}
			default:
				t.Error("too many historical sends")
			}
			writeFixture(t, writer, fmt.Sprintf(`{"ok":true,"result":{"message_id":%d}}`, 500+sends))
		default:
			t.Error("history called unexpected/write endpoint")
			writer.WriteHeader(400)
		}
	}))
	defer server.Close()
	config := Config{VKAccessToken: "fake-token", VKTargetPeerID: 2000000001, TelegramBotToken: "fake-token", TelegramTargetChatID: -123}
	vk, err := NewVKClient(config, server.URL, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	telegram, err := NewTelegramClient(config, server.URL, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	relay := Relay{config: config, vk: vk, telegram: telegram, store: store, mediaHTTP: server.Client(), logger: slog.New(slog.NewTextHandler(io.Discard, nil)), tempRoot: t.TempDir()}
	if err := relay.ReplayHistory(t.Context(), 3); err != nil {
		t.Fatal(err)
	}
	if sends != 3 {
		t.Fatal("history incorrectly suppressed existing mappings")
	}
	for _, pair := range []struct{ vk, tg int64 }{{1, 9001}, {2, 0}, {3, 0}, {99, 9099}} {
		identifier, err := store.Lookup(t.Context(), pair.vk)
		if err != nil || identifier != pair.tg {
			t.Fatal("manual replay corrupted normal mapping")
		}
	}
}

func TestForwardAndWallAreNotReplies(t *testing.T) {
	var message VKMessage
	if err := json.Unmarshal([]byte(`{"id":42,"fwd_messages":[{"id":1}],"attachments":[{"type":"wall","wall":{"id":1,"owner_id":-42,"text":"wall"}}]}`), &message); err != nil {
		t.Fatal(err)
	}
	rendered, err := normalizeMessage(message, "Sender")
	if err != nil || rendered.VKReplyToID != 0 || !rendered.Repost || rendered.IgnoredForwards != 1 {
		t.Fatal("forward or wall confused with reply")
	}
}
