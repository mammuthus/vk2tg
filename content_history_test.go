package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestHistoryStickerLinkAndService(t *testing.T) {
	for _, dry := range []bool{false, true} {
		t.Run(fmt.Sprint(dry), func(t *testing.T) {
			var methods []string
			lookups, downloads := 0, 0
			var logs bytes.Buffer
			var server *httptest.Server
			server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				switch request.URL.Path {
				case "/messages.getHistory":
					if request.ParseForm() != nil || request.PostForm.Get("v") != VKAPIVersion {
						t.Error("history version changed")
					}
					writeFixture(t, writer, fmt.Sprintf(`{"response":{"items":[
					{"id":8,"peer_id":2000000001,"from_id":42,"attachments":[{"type":"audio"}]},
					{"id":7,"peer_id":2000000001,"from_id":42,"attachments":[{"type":"link","link":{"url":"https://example.test/only","title":"Title"}}]},
					{"id":6,"peer_id":2000000001,"from_id":42,"text":" \n "},
					{"id":5,"peer_id":2000000001,"from_id":42,"action":{"type":"chat_pin_message"},"attachments":[{"type":"photo","photo":{"sizes":[{"url":%q,"width":100,"height":100}]}}]},
					{"id":4,"peer_id":2000000001,"from_id":42,"action":{"type":"chat_pin_message"},"text":"useful content"},
					{"id":3,"peer_id":2000000001,"from_id":42,"text":"","attachments":[],"action":{"type":"chat_pin_message","member_id":42,"conversation_message_id":2}},
					{"id":2,"peer_id":2000000001,"from_id":42,"text":"See https://example.test/lesson","attachments":[{"type":"link","link":{"url":"https://example.test/lesson","title":"Lesson"}}]},
					{"id":1,"peer_id":2000000001,"from_id":42,"text":"","attachments":[{"type":"sticker","sticker":{"inner_type":"base_sticker","sticker_id":10,"product_id":20,"is_allowed":true}}]}
					]}}`, server.URL+"/photo"))
				case "/messages.getById":
					lookups++
					if request.ParseForm() != nil || request.PostForm.Get("v") != "5.131" || request.PostForm.Get("message_ids") != "1" {
						t.Error("incorrect sticker compatibility lookup")
					}
					writeFixture(t, writer, fmt.Sprintf(`{"response":{"items":[{"id":1,"peer_id":2000000001,"from_id":42,"text":"must not replace original content","attachments":[{"type":"sticker","sticker":{"inner_type":"base_sticker","sticker_id":10,"product_id":20,"is_allowed":true,"images":[{"url":%q,"width":64,"height":64},{"url":%q,"width":512,"height":512}],"images_with_background":[{"url":%q,"width":512,"height":512}]}}]}]}}`, server.URL+"/small", server.URL+"/transparent", server.URL+"/sticker"))
				case "/users.get":
					writeFixture(t, writer, `{"response":[{"id":42,"first_name":"Sender","last_name":""}]}`)
				case "/photo", "/sticker":
					downloads++
					writeFixture(t, writer, "image-bytes")
				case "/botfake-token/sendPhoto":
					methods = append(methods, "sendPhoto")
					if request.ParseMultipartForm(1<<20) != nil {
						t.Error("invalid photo multipart")
						return
					}
					defer request.MultipartForm.RemoveAll()
					caption := request.FormValue("caption")
					if strings.Contains(caption, relayFooter) || strings.Contains(caption, "must not replace") {
						t.Error("sticker caption/footer changed")
					}
					file, header, err := request.FormFile("photo")
					if err != nil {
						t.Error("missing uploaded image")
						return
					}
					file.Close()
					if len(methods) == 1 && header.Filename != "sticker.png" {
						t.Error("sticker did not use photo pipeline")
					}
					writeTelegramSuccess(t, writer, request)
				case "/botfake-token/sendMessage":
					methods = append(methods, "sendMessage")
					var payload struct {
						Text string `json:"text"`
					}
					if json.NewDecoder(request.Body).Decode(&payload) != nil {
						t.Error("invalid text send")
					}
					want := map[int]string{2: "See https://example.test/lesson", 3: "useful content", 5: "https://example.test/only", 6: "[Unsupported attachment]"}[len(methods)]
					if want == "" || payload.Text != renderChunks("Sender", false, want, 4096, "")[0] {
						t.Error("empty service send, duplicate link, lost content or unknown fallback regression")
					}
					writeTelegramSuccess(t, writer, request)
				default:
					t.Error("unexpected endpoint, small sticker download, or VK write")
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
			if err := relay.ReplayHistory(t.Context(), 8); err != nil {
				t.Fatal(err)
			}
			if lookups != 1 {
				t.Fatal("sticker enrichment not scoped to missing image")
			}
			if dry {
				if downloads != 0 || len(methods) != 0 {
					t.Fatal("dry-run downloaded or sent media")
				}
			} else if downloads != 2 || !reflect.DeepEqual(methods, []string{"sendPhoto", "sendMessage", "sendMessage", "sendPhoto", "sendMessage", "sendMessage"}) {
				t.Fatal("incorrect delivery order/count")
			}
			if !strings.Contains(logs.String(), `"skipped":2`) {
				t.Fatal("empty records not counted as skipped")
			}
			entries, err := os.ReadDir(directory)
			if err != nil || len(entries) != 0 {
				t.Fatal("media cleanup failed")
			}
		})
	}
}

func TestStickerLookupRejectsMismatch(t *testing.T) {
	for _, body := range []string{
		`{"response":{"items":[]}}`,
		`{"response":{"items":[{"id":1,"peer_id":2,"from_id":3,"attachments":[{"type":"sticker","sticker":{"sticker_id":99,"images":[{"url":"https://example.test/photo","width":512,"height":512}]}}]}]}}`,
		`{"response":{"items":[{"id":1,"peer_id":99,"from_id":3}]}}`,
		`{"response":{"items":[{"id":1,"peer_id":2,"from_id":3,"attachments":[{"type":"sticker","sticker":{"sticker_id":42}}]}]}}`,
		`{"error":{"error_code":5,"error_msg":"private"}}`,
	} {
		t.Run(body, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) { writeFixture(t, writer, body) }))
			defer server.Close()
			client, err := NewVKClient(Config{VKAccessToken: "fake-token"}, server.URL, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			message := VKMessage{ID: 1, PeerID: 2, FromID: 3, Attachments: []VKAttachment{{Type: "sticker", Sticker: &VKSticker{ID: 42}}}}
			err = client.enrichStickers(t.Context(), message)
			if err == nil || strings.Contains(err.Error(), "private") {
				t.Fatal("invalid enrichment accepted or leaked")
			}
		})
	}
}
