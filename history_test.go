package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestHistoryReplay(t *testing.T) {
	var sent []string
	var logs bytes.Buffer
	metadata, historyCalls := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/messages.getHistory":
			historyCalls++
			if err := request.ParseForm(); err != nil {
				t.Error(err)
			}
			if request.PostForm.Get("peer_id") != "2000000001" || request.PostForm.Get("count") != "3" || request.PostForm.Get("offset") != "0" || request.PostForm.Get("rev") != "0" {
				t.Error("incorrect latest-history request")
			}
			if historyCalls == 1 {
				writeFixture(t, writer, `{"error":{"error_code":6,"error_msg":"too many requests"}}`)
				return
			}
			writeFixture(t, writer, `{"response":{"items":[{"id":3,"peer_id":2000000001,"from_id":42,"text":"new <&>"},{"id":2,"peer_id":2000000001,"from_id":73,"text":"blocked"},{"id":1,"peer_id":2000000001,"from_id":42,"out":1,"text":"owner"}]}}`)
		case "/users.get":
			metadata++
			if err := request.ParseForm(); err != nil {
				t.Error(err)
			}
			if request.PostForm.Get("user_ids") != "42" {
				t.Error("blocked metadata lookup")
			}
			writeFixture(t, writer, `{"response":[{"id":42,"first_name":"A <B>","last_name":""}]}`)
		case "/botfake-token/sendMessage":
			var payload struct {
				Text string `json:"text"`
			}
			if json.NewDecoder(request.Body).Decode(&payload) != nil {
				t.Error("invalid Telegram payload")
			}
			sent = append(sent, payload.Text)
			writeTelegramSuccess(t, writer, request)
		default:
			t.Errorf("unexpected or VK write endpoint: %s", request.URL.Path)
			writer.WriteHeader(400)
		}
	}))
	defer server.Close()
	config := Config{VKAccessToken: "fake-token", VKTargetPeerID: 2000000001, TelegramBotToken: "fake-token", TelegramTargetChatID: -123, VKBlockedSenderIDs: map[int64]struct{}{73: {}}}
	vk, err := NewVKClient(config, server.URL, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	vk.rate.maxRetries = vkAPIMaxRetries
	vk.rate.clock = &fakeVKClock{now: time.Unix(1_900_000_000, 0)}
	vk.rate.jitter = func(delay time.Duration) time.Duration { return delay }
	telegram, err := NewTelegramClient(config, server.URL, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	relay := Relay{config: config, vk: vk, telegram: telegram, mediaHTTP: server.Client(), logger: slog.New(slog.NewJSONHandler(&logs, nil)), retryDelay: time.Millisecond}
	if err := relay.ReplayHistory(t.Context(), 3); err != nil {
		t.Fatal(err)
	}
	if relay.retryDelay != time.Millisecond {
		t.Fatal("history replay changed configured retry delay")
	}
	want := []string{renderChunks("A <B>", false, "owner", 4096, "")[0], renderChunks("A <B>", false, "new <&>", 4096, "")[0]}
	if !reflect.DeepEqual(sent, want) || metadata != 1 || historyCalls != 2 {
		t.Fatalf("history order, renderer, blocklist or retry failed; sends=%d metadata=%d history=%d", len(sent), metadata, historyCalls)
	}
	if !strings.Contains(logs.String(), "history message inspected") || !strings.Contains(logs.String(), "\"message_id\":1") || !strings.Contains(logs.String(), "\"outbox\":true") || !strings.Contains(logs.String(), "\"blocked_sender\":true") || !strings.Contains(logs.String(), "\"reason\":\"blocked_sender\"") {
		t.Fatal("history diagnostics missing exact safe message facts")
	}
}

func TestHistoryFloodStopsAfterOneRequest(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests++
		writeFixture(t, writer, `{"error":{"error_code":9,"error_msg":"flood control"}}`)
	}))
	defer server.Close()
	config := Config{VKAccessToken: "fake-token", VKTargetPeerID: 2000000001}
	vk, err := NewVKClient(config, server.URL, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	relay := Relay{config: config, vk: vk, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	err = relay.ReplayHistory(t.Context(), 5)
	var apiError *VKAPIError
	if !errors.As(err, &apiError) || apiError.Code != 9 || requests != 1 {
		t.Fatalf("history flood handling: requests=%d err=%v", requests, err)
	}
}

func TestHistoryMediaPipeline(t *testing.T) {
	for _, dry := range []bool{false, true} {
		t.Run(fmt.Sprint(dry), func(t *testing.T) {
			var methods []string
			downloads := 0
			var logs bytes.Buffer
			var server *httptest.Server
			server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				switch request.URL.Path {
				case "/messages.getHistory":
					photo := fmt.Sprintf(`{"type":"photo","photo":{"sizes":[{"url":%q,"width":100,"height":100}]}}`, server.URL+"/media")
					writeFixture(t, writer, fmt.Sprintf(`{"response":{"items":[{"id":3,"peer_id":2000000001,"from_id":42,"attachments":[{"type":"doc","doc":{"url":%q,"title":"file.txt"}}]},{"id":2,"peer_id":2000000001,"from_id":42,"attachments":[%s,%s]},{"id":1,"peer_id":2000000001,"from_id":42,"out":1,"attachments":[{"type":"wall","wall":{"copy_history":[{"id":9,"owner_id":-42,"text":"wall <text>\nnext line","attachments":[%s]}]}}]}]}}`, server.URL+"/media", photo, photo, photo))
				case "/users.get":
					writeFixture(t, writer, `{"response":[{"id":42,"first_name":"Sender","last_name":""}]}`)
				case "/media":
					downloads++
					writeFixture(t, writer, "history-file")
				case "/botfake-token/sendPhoto", "/botfake-token/sendMediaGroup", "/botfake-token/sendDocument":
					method := strings.TrimPrefix(request.URL.Path, "/botfake-token/")
					methods = append(methods, method)
					if request.ParseMultipartForm(1<<20) != nil {
						t.Error("not a real multipart upload")
						writer.WriteHeader(400)
						return
					}
					defer request.MultipartForm.RemoveAll()
					caption := request.FormValue("caption")
					if method == "sendMediaGroup" {
						var items []struct {
							Caption string `json:"caption"`
							Media   string `json:"media"`
						}
						if json.Unmarshal([]byte(request.FormValue("media")), &items) != nil || len(items) != 2 {
							t.Error("invalid history album")
							return
						}
						for index, item := range items {
							if strings.Contains(item.Caption, relayFooter) || !strings.HasPrefix(item.Media, "attach://") || (index > 0 && item.Caption != "") {
								t.Error("album has footer or bypasses normal upload")
							}
						}
					} else if strings.Contains(caption, relayFooter) {
						t.Error("unexpected history footer")
					}
					if method == "sendPhoto" && (!strings.HasPrefix(caption, "<b>Sender</b> (<a href=\"https://vk.ru/wall-42_9\">репост</a>)\n\n") || !strings.Contains(caption, "wall &lt;text&gt;\nnext line") || strings.Count(caption, "https://vk.ru/wall-42_9") != 1) {
						t.Error("history bypasses wall renderer")
					}
					for _, files := range request.MultipartForm.File {
						for _, header := range files {
							file, err := header.Open()
							if err != nil {
								t.Error(err)
								continue
							}
							data, err := io.ReadAll(file)
							file.Close()
							if err != nil || string(data) != "history-file" {
								t.Error("incorrect historical upload")
							}
						}
					}
					writeTelegramSuccess(t, writer, request)
				default:
					t.Errorf("unexpected endpoint (history must not poll/write VK): %s", request.URL.Path)
					writer.WriteHeader(400)
				}
			}))
			defer server.Close()
			config := Config{VKAccessToken: "fake-token", VKTargetPeerID: 2000000001, TelegramBotToken: "fake-token", TelegramTargetChatID: -123, DryRun: dry}
			vk, err := NewVKClient(config, server.URL, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			telegram, err := NewTelegramClient(config, server.URL, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			directory := t.TempDir()
			relay := Relay{config: config, vk: vk, telegram: telegram, mediaHTTP: server.Client(), tempRoot: directory, logger: slog.New(slog.NewJSONHandler(&logs, nil))}
			if err := relay.ReplayHistory(t.Context(), 3); err != nil {
				t.Fatal(err)
			}
			if dry {
				if len(methods) != 0 || downloads != 0 {
					t.Fatal("dry-run did external send/media work")
				}
			} else if !reflect.DeepEqual(methods, []string{"sendPhoto", "sendMediaGroup", "sendDocument"}) || downloads != 4 {
				t.Fatal("history skipped media path or chronology")
			}
			entries, err := os.ReadDir(directory)
			if err != nil || len(entries) != 0 {
				t.Fatal("history leaked temp files")
			}
			if strings.Contains(logs.String(), "wall <text>") || strings.Contains(logs.String(), "fake-token") {
				t.Fatal("history log exposed content")
			}
		})
	}
}

func TestHistoryCommand(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		args    []string
		count   int
		invalid bool
	}{
		{name: "runtime"},
		{name: "default history", args: []string{"test-history"}, count: 3},
		{name: "explicit count", args: []string{"test-history", "--count", "3"}, count: 3},
		{name: "zero", args: []string{"test-history", "--count", "0"}, invalid: true},
		{name: "too many", args: []string{"test-history", "--count", "101"}, invalid: true},
		{name: "typo", args: []string{"test-histroy"}, invalid: true},
		{name: "positional", args: []string{"test-history", "3"}, invalid: true},
		{name: "unknown flag", args: []string{"test-history", "--token", "private-value"}, invalid: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			count, err := parseHistoryCommand(testCase.args)
			if (err != nil) != testCase.invalid || (!testCase.invalid && count != testCase.count) {
				t.Fatal("incorrect command parsing")
			}
			if err != nil && strings.Contains(err.Error(), "private-value") {
				t.Fatal("CLI echoed unknown input")
			}
		})
	}
}
