package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRelayEndToEnd(t *testing.T) {
	tests := []struct {
		name                string
		peer, flags, sender int
		attachments         string
		dry                 bool
		wantSend, wantFull  int
	}{
		{name: "ordinary text", peer: 2000000001, sender: 42, wantSend: 1},
		{name: "wrong peer", peer: 2000000002, sender: 42, attachments: `{"attach1_type":"photo"}`},
		{name: "outbox", peer: 2000000001, sender: 42, flags: 2, attachments: `{"attach1_type":"photo"}`},
		{name: "blocked sender", peer: 2000000001, sender: 73, attachments: `{"attach1_type":"photo"}`},
		{name: "dry run", peer: 2000000001, sender: 42, dry: true},
		{name: "dry run attachment", peer: 2000000001, sender: 42, dry: true, attachments: `{"attach1_type":"photo"}`, wantFull: 1},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			polls, sends, metadata, fullCalls, downloads := 0, 0, 0, 0, 0
			var log bytes.Buffer
			var server *httptest.Server
			server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				switch request.URL.Path {
				case "/messages.getLongPollServer":
					writeFixture(t, writer, fmt.Sprintf(`{"response":{"server":%q,"key":"fake-key","ts":"10"}}`, server.URL+"/poll"))
				case "/poll":
					polls++
					if polls > 1 {
						if request.URL.Query().Get("ts") != "11" {
							t.Error("cursor not advanced")
						}
						writeFixture(t, writer, `{"failed":4}`)
						return
					}
					attachments := testCase.attachments
					if attachments == "" {
						attachments = "{}"
					}
					writeFixture(t, writer, fmt.Sprintf(`{"ts":"11","updates":[[4,99,%d,%d,123,"hello &lt;&amp;&gt;",{"from":"%d"},%s]]}`, testCase.flags, testCase.peer, testCase.sender, attachments))
				case "/users.get":
					metadata++
					writeFixture(t, writer, `{"response":[{"id":42,"first_name":"A <B>","last_name":"User"}]}`)
				case "/messages.getById":
					fullCalls++
					writeFixture(t, writer, fmt.Sprintf(`{"response":{"items":[{"id":99,"peer_id":2000000001,"from_id":42,"text":"full text","attachments":[{"type":"photo","photo":{"sizes":[{"url":%q,"width":100,"height":100}]}}]}]}}`, server.URL+"/media"))
				case "/media":
					downloads++
					writeFixture(t, writer, "photo")
				case "/botfake-bot-token/sendMessage":
					sends++
					var payload struct {
						Text string `json:"text"`
					}
					if json.NewDecoder(request.Body).Decode(&payload) != nil || payload.Text != "<b>A &lt;B&gt; User</b>\n\nhello &lt;&amp;&gt;" {
						t.Error("incorrect relayed text")
					}
					writeTelegramSuccess(t, writer, request)
				default:
					t.Errorf("unexpected or write endpoint: %s", request.URL.Path)
					writer.WriteHeader(400)
				}
			}))
			defer server.Close()
			config := Config{VKAccessToken: "fake-token", VKTargetPeerID: 2000000001, TelegramBotToken: "fake-bot-token", TelegramTargetChatID: -123, VKBlockedSenderIDs: map[int64]struct{}{73: {}}, DryRun: testCase.dry}
			vk, err := NewVKClient(config, server.URL, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			telegram, err := newTestTelegramClient(config, server.URL, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			relay := Relay{config: config, vk: vk, telegram: telegram, store: testMessageStore(t), mediaHTTP: server.Client(), logger: slog.New(slog.NewJSONHandler(&log, nil)), tempRoot: t.TempDir(), retryDelay: time.Millisecond}
			if err := relay.Run(t.Context()); err == nil {
				t.Fatal("failed=4 should stop the loop")
			}
			if sends != testCase.wantSend || fullCalls != testCase.wantFull || downloads != 0 {
				t.Fatalf("unexpected work: sends=%d full=%d downloads=%d", sends, fullCalls, downloads)
			}
			if testCase.wantSend == 0 && !testCase.dry && metadata != 0 {
				t.Fatal("filter performed metadata lookup")
			}
			if strings.Contains(log.String(), "hello") || strings.Contains(log.String(), "fake-token") || strings.Contains(log.String(), "full text") {
				t.Fatal("unsafe log")
			}
			if testCase.dry && !strings.Contains(log.String(), "would relay message") {
				t.Fatal("missing dry-run fact")
			}
		})
	}
}

func TestRelayFailedResponses(t *testing.T) {
	for _, failed := range []int{1, 2, 3} {
		t.Run(fmt.Sprint(failed), func(t *testing.T) {
			acquisitions, polls := 0, 0
			var server *httptest.Server
			server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				switch request.URL.Path {
				case "/messages.getLongPollServer":
					acquisitions++
					writeFixture(t, writer, fmt.Sprintf(`{"response":{"server":%q,"key":"key%d","ts":"%d"}}`, server.URL+"/poll", acquisitions, acquisitions*10))
				case "/poll":
					polls++
					if polls == 1 {
						writeFixture(t, writer, fmt.Sprintf(`{"failed":%d,"ts":"15"}`, failed))
						return
					}
					wantTS, wantKey := "15", "key1"
					if failed == 2 {
						wantTS, wantKey = "10", "key2"
					}
					if failed == 3 {
						wantTS, wantKey = "20", "key2"
					}
					if request.URL.Query().Get("ts") != wantTS || request.URL.Query().Get("key") != wantKey {
						t.Error("incorrect cursor/key recovery")
					}
					writeFixture(t, writer, `{"failed":4}`)
				default:
					t.Error("unexpected VK endpoint")
					writer.WriteHeader(400)
				}
			}))
			defer server.Close()
			vk, err := NewVKClient(Config{VKAccessToken: "fake-token"}, server.URL, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			relay := Relay{vk: vk, logger: slog.New(slog.NewTextHandler(io.Discard, nil)), retryDelay: time.Millisecond}
			if relay.Run(t.Context()) == nil {
				t.Fatal("expected protocol stop")
			}
			if polls != 2 {
				t.Fatal("did not resume polling")
			}
			want := 2
			if failed == 1 {
				want = 1
			}
			if acquisitions != want {
				t.Fatal("unexpected server refresh")
			}
		})
	}
}

func TestRelayFatalVKErrors(t *testing.T) {
	for _, code := range []int{5, 14, 17, 25} {
		for _, stage := range []string{"server", "message", "sender"} {
			t.Run(fmt.Sprintf("%d/%s", code, stage), func(t *testing.T) {
				fatalCalls := 0
				var server *httptest.Server
				server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
					fatalPath := map[string]string{"server": "/messages.getLongPollServer", "message": "/messages.getById", "sender": "/users.get"}[stage]
					if request.URL.Path == fatalPath {
						fatalCalls++
						writeFixture(t, writer, fmt.Sprintf(`{"error":{"error_code":%d,"error_msg":"fake-token"}}`, code))
						return
					}
					switch request.URL.Path {
					case "/messages.getLongPollServer":
						writeFixture(t, writer, fmt.Sprintf(`{"response":{"server":%q,"key":"fake-key","ts":"10"}}`, server.URL+"/poll"))
					case "/poll":
						attachment := "{}"
						if stage == "message" {
							attachment = `{"attach1_type":"photo"}`
						}
						writeFixture(t, writer, fmt.Sprintf(`{"ts":"11","updates":[[4,99,0,2000000001,123,"text",{"from":"42"},%s]]}`, attachment))
					default:
						t.Error("unexpected endpoint")
						writer.WriteHeader(400)
					}
				}))
				defer server.Close()
				config := Config{VKAccessToken: "fake-token", VKTargetPeerID: 2000000001, DryRun: true}
				vk, err := NewVKClient(config, server.URL, time.Second)
				if err != nil {
					t.Fatal(err)
				}
				relay := Relay{config: config, vk: vk, logger: slog.New(slog.NewTextHandler(io.Discard, nil)), retryDelay: time.Millisecond}
				var failure *VKAPIError
				err = relay.Run(t.Context())
				if !errors.As(err, &failure) || failure.Code != code || fatalCalls != 1 {
					t.Fatalf("fatal VK error was not propagated: %v", err)
				}
			})
		}
	}
}

func TestRelayTransientAndCancellation(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	acquisitions, polls := 0, 0
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/messages.getLongPollServer" {
			acquisitions++
			if acquisitions == 1 {
				writer.WriteHeader(500)
				return
			}
			writeFixture(t, writer, fmt.Sprintf(`{"response":{"server":%q,"key":"key","ts":"10"}}`, server.URL+"/poll"))
			return
		}
		if request.URL.Path != "/poll" {
			t.Error("unexpected endpoint")
			writer.WriteHeader(400)
			return
		}
		polls++
		if request.URL.Query().Get("ts") != "10" {
			t.Error("retry changed cursor")
		}
		if polls == 1 {
			writer.WriteHeader(502)
			return
		}
		cancel()
		writeFixture(t, writer, `{"ts":"11","updates":[]}`)
	}))
	defer server.Close()
	vk, err := NewVKClient(Config{VKAccessToken: "fake-token"}, server.URL, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	relay := Relay{vk: vk, logger: slog.New(slog.NewTextHandler(io.Discard, nil)), retryDelay: time.Millisecond}
	if err := relay.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("unexpected shutdown: %v", err)
	}
	if acquisitions != 2 || polls != 2 {
		t.Fatal("transient error was not retried")
	}
}

func TestRelayWallPhoto(t *testing.T) {
	polls, photos := 0, 0
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
			writeFixture(t, writer, `{"ts":"11","updates":[[4,99,0,2000000001,123,"",{"from":"42"},{"attach1_type":"wall"}]]}`)
		case "/messages.getById":
			writeFixture(t, writer, fmt.Sprintf(`{"response":{"items":[{"id":99,"peer_id":2000000001,"from_id":42,"attachments":[{"type":"wall","wall":{"id":7,"owner_id":-42,"text":"wall <text>\nsecond line","attachments":[{"type":"photo","photo":{"sizes":[{"url":%q,"width":100,"height":100}]}}]}}]}]}}`, server.URL+"/photo"))
		case "/users.get":
			writeFixture(t, writer, `{"response":[{"id":42,"first_name":"Sender","last_name":""}]}`)
		case "/photo":
			writeFixture(t, writer, "fake-photo")
		case "/botfake-token/sendPhoto":
			photos++
			if request.ParseMultipartForm(1<<20) != nil {
				t.Error("missing photo upload")
				return
			}
			defer request.MultipartForm.RemoveAll()
			caption := request.FormValue("caption")
			if !strings.HasPrefix(caption, "<b>Sender</b> (<a href=\"https://vk.ru/wall-42_7\">репост</a>)\n\n") || !strings.Contains(caption, "wall &lt;text&gt;\nsecond line") || strings.Count(caption, "https://vk.ru/wall-42_7") != 1 || strings.Contains(caption, relayFooter) {
				t.Error("incorrect wall caption")
			}
			writeTelegramSuccess(t, writer, request)
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
	telegram, err := newTestTelegramClient(config, server.URL, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	relay := Relay{config: config, vk: vk, telegram: telegram, store: testMessageStore(t), mediaHTTP: server.Client(), tempRoot: t.TempDir(), logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if relay.Run(t.Context()) == nil || photos != 1 {
		t.Fatal("wall photo was not delivered")
	}
}
