package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDeliveryCanonicalAndReply(t *testing.T) {
	for _, kind := range []string{"text", "photo document", "album", "document", "long caption", "late failure", "late 404"} {
		t.Run(kind, func(t *testing.T) {
			calls := 0
			var methods []string
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.URL.Path == "/file" {
					writeFixture(t, writer, "file-bytes")
					return
				}
				calls++
				method := strings.TrimPrefix(request.URL.Path, "/botfake-token/")
				methods = append(methods, method)
				var reply TelegramReplyParameters
				if method == "sendMessage" {
					var payload struct {
						Reply TelegramReplyParameters `json:"reply_parameters"`
						Text  string                  `json:"text"`
					}
					if json.NewDecoder(request.Body).Decode(&payload) != nil {
						t.Error("invalid text request")
					}
					reply = payload.Reply
					if strings.Contains(payload.Text, relayFooter) {
						t.Error("unexpected continuation footer")
					}
				} else {
					if request.ParseMultipartForm(1<<20) != nil {
						t.Error("invalid multipart body")
						return
					}
					defer request.MultipartForm.RemoveAll()
					if json.Unmarshal([]byte(request.FormValue("reply_parameters")), &reply) != nil {
						t.Error("missing media reply")
					}
				}
				if reply.MessageID != 77 || !reply.AllowSendingWithoutReply {
					t.Error("incorrect reply target")
				}
				if (kind == "late failure" || kind == "late 404") && calls == 2 {
					status := 500
					if kind == "late 404" {
						status = 404
					}
					writer.WriteHeader(status)
					writeFixture(t, writer, fmt.Sprintf(`{"ok":false,"error_code":%d}`, status))
					return
				}
				identifier := 100 + calls
				if method == "sendMediaGroup" {
					writeFixture(t, writer, fmt.Sprintf(`{"ok":true,"result":[{"message_id":%d},{"message_id":999}]}`, identifier))
				} else {
					writeFixture(t, writer, fmt.Sprintf(`{"ok":true,"result":{"message_id":%d}}`, identifier))
				}
			}))
			defer server.Close()
			client, err := newTestTelegramClient(Config{TelegramBotToken: "fake-token", TelegramTargetChatID: -123}, server.URL, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			message := RenderedMessage{Name: "Sender", Text: "text", TelegramReplyID: 77}
			photo := MediaSource{Kind: "photo", URL: server.URL + "/file", Name: "photo.jpg"}
			document := MediaSource{Kind: "document", URL: server.URL + "/file", Name: "doc.txt"}
			switch kind {
			case "photo document", "late failure", "late 404":
				message.Media = []MediaSource{photo, document}
			case "album":
				message.Media = []MediaSource{photo, photo}
			case "document":
				message.Media = []MediaSource{document}
			case "long caption":
				message.Media = []MediaSource{photo}
				message.Text = strings.Repeat("x", 6000)
			}
			relay := Relay{telegram: client, mediaHTTP: server.Client(), tempRoot: t.TempDir()}
			identifier, err := relay.deliverWithMediaRetry(t.Context(), &message)
			if kind == "late failure" || kind == "late 404" {
				if err == nil || identifier != 0 || calls != 2 {
					t.Fatal("failed multi-part send returned canonical success")
				}
				return
			}
			if err != nil || identifier != 101 {
				t.Fatalf("canonical must be first primary Telegram message: %d %v", identifier, err)
			}
			if kind == "photo document" && strings.Join(methods, ",") != "sendPhoto,sendDocument" {
				t.Fatal("wrong multi-media order")
			}
		})
	}
}

func TestMediaDelivery(t *testing.T) {
	for _, kind := range []string{"photo", "album", "document", "long caption", "send failure"} {
		t.Run(kind, func(t *testing.T) {
			mediaServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) { writeFixture(t, writer, "fake-media-bytes") }))
			defer mediaServer.Close()
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				calls++
				if strings.HasSuffix(request.URL.Path, "sendMessage") {
					var payload struct {
						Text string `json:"text"`
					}
					if json.NewDecoder(request.Body).Decode(&payload) != nil || strings.Contains(payload.Text, relayFooter) {
						t.Error("invalid continuation")
					}
				} else {
					if err := request.ParseMultipartForm(1 << 20); err != nil {
						t.Error(err)
						return
					}
					defer request.MultipartForm.RemoveAll()
					if kind == "album" {
						if !strings.HasSuffix(request.URL.Path, "sendMediaGroup") {
							t.Error("expected media group")
						}
						var items []struct {
							Caption string `json:"caption"`
							Media   string `json:"media"`
						}
						if json.Unmarshal([]byte(request.FormValue("media")), &items) != nil || len(items) != 2 {
							t.Error("invalid album")
							return
						}
						for index, item := range items {
							if strings.Contains(item.Caption, relayFooter) || !strings.HasPrefix(item.Media, "attach://") || (index > 0 && item.Caption != "") {
								t.Error("unexpected album caption or missing upload")
							}
						}
					} else {
						method := "sendPhoto"
						if kind == "document" {
							method = "sendDocument"
						}
						if !strings.HasSuffix(request.URL.Path, method) || strings.Contains(request.FormValue("caption"), relayFooter) {
							t.Error("invalid media method/caption")
						}
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
							if err != nil || string(data) != "fake-media-bytes" {
								t.Error("media was not uploaded as file")
							}
						}
					}
				}
				if kind == "send failure" {
					writer.WriteHeader(400)
					writeFixture(t, writer, `{"ok":false,"error_code":400}`)
					return
				}
				writeTelegramSuccess(t, writer, request)
			}))
			defer server.Close()
			client, err := newTestTelegramClient(Config{TelegramBotToken: "fake-token", TelegramTargetChatID: -123}, server.URL, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			message := RenderedMessage{Name: "Sender", Text: "photo text", Media: []MediaSource{{Kind: "photo", URL: mediaServer.URL, Name: "photo.jpg"}}}
			if kind == "album" {
				message.Media = append(message.Media, message.Media[0])
			}
			if kind == "document" {
				message.Media[0].Kind = "document"
				message.Media[0].Name = "report.txt"
			}
			if kind == "long caption" {
				message.Text = strings.Repeat("x", 5000)
			}
			directory := t.TempDir()
			_, err = deliverMessage(t.Context(), client, mediaServer.Client(), directory, message)
			if (err != nil) != (kind == "send failure") {
				t.Fatalf("unexpected send result: %v", err)
			}
			entries, err := os.ReadDir(directory)
			if err != nil || len(entries) != 0 {
				t.Fatal("temporary files leaked")
			}
			if calls == 0 || (kind == "long caption" && calls < 2) {
				t.Fatal("missing delivery")
			}
		})
	}
}

func TestMediaRateLimitReplay(t *testing.T) {
	mediaServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) { writeFixture(t, writer, "replay-file") }))
	defer mediaServer.Close()
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls++
		if request.ContentLength <= 0 {
			t.Error("multipart upload must have a known content length")
		}
		if request.ParseMultipartForm(1<<20) != nil {
			t.Error("invalid repeated multipart body")
			writer.WriteHeader(400)
			return
		}
		defer request.MultipartForm.RemoveAll()
		file, _, err := request.FormFile("photo")
		if err != nil {
			t.Error("missing repeated upload")
			writer.WriteHeader(400)
			return
		}
		data, err := io.ReadAll(file)
		file.Close()
		if err != nil || string(data) != "replay-file" {
			t.Error("corrupt repeated upload")
		}
		if calls == 1 {
			writer.WriteHeader(429)
			writeFixture(t, writer, `{"ok":false,"error_code":429,"parameters":{"retry_after":1}}`)
			return
		}
		writeTelegramSuccess(t, writer, request)
	}))
	defer server.Close()
	client, err := newTestTelegramClient(Config{TelegramBotToken: "fake-token", TelegramTargetChatID: -123}, server.URL, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	_, err = deliverMessage(t.Context(), client, mediaServer.Client(), directory, RenderedMessage{Name: "Sender", Media: []MediaSource{{Kind: "photo", URL: mediaServer.URL, Name: "photo.jpg"}}})
	if err != nil || calls != 2 {
		t.Fatalf("multipart retry failed: %v", err)
	}
	entries, err := os.ReadDir(directory)
	if err != nil || len(entries) != 0 {
		t.Fatal("retry leaked files")
	}
}

func TestMediaDownloadCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) { cancel(); <-request.Context().Done() }))
	defer server.Close()
	directory := t.TempDir()
	_, err := deliverMessage(ctx, nil, server.Client(), directory, RenderedMessage{Name: "Sender", Media: []MediaSource{{Kind: "photo", URL: server.URL, Name: "photo.jpg"}}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("unexpected error: %v", err)
	}
	entries, err := os.ReadDir(directory)
	if err != nil || len(entries) != 0 {
		t.Fatal("cancellation leaked files")
	}
}

type cancelMediaRetryWriter struct{ cancel context.CancelFunc }

func (writer cancelMediaRetryWriter) Write(data []byte) (int, error) {
	writer.cancel()
	return len(data), nil
}

func TestMediaRefreshFailureBoundaries(t *testing.T) {
	for _, testCase := range []struct {
		name                               string
		status, vkCode, downloads, refresh int
		cancel                             bool
		refreshChange                      string
	}{
		{name: "404 exhausted", status: 404, downloads: 4, refresh: 3},
		{name: "403 exhausted", status: 403, downloads: 4, refresh: 3},
		{name: "400 not retried", status: 400, downloads: 1},
		{name: "VK flood 9", status: 404, vkCode: 9, downloads: 1, refresh: 1},
		{name: "VK flood 29", status: 404, vkCode: 29, downloads: 1, refresh: 1},
		{name: "cancel backoff", status: 404, downloads: 1, cancel: true},
		{name: "wrong peer on refresh", status: 404, downloads: 1, refresh: 1, refreshChange: "peer"},
		{name: "blocked sender on refresh", status: 404, downloads: 1, refresh: 1, refreshChange: "sender"},
		{name: "removed attachment", status: 404, downloads: 1, refresh: 1, refreshChange: "empty"},
		{name: "changed attachment type", status: 404, downloads: 1, refresh: 1, refreshChange: "type"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			downloads, refresh, sends := 0, 0, 0
			var server *httptest.Server
			server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				switch request.URL.Path {
				case "/file":
					downloads++
					writer.WriteHeader(testCase.status)
				case "/messages.getById":
					refresh++
					if testCase.vkCode != 0 {
						writeFixture(t, writer, fmt.Sprintf(`{"error":{"error_code":%d}}`, testCase.vkCode))
						return
					}
					message := map[string]any{
						"id": 99, "peer_id": 2000000001, "from_id": 42, "out": 1,
						"attachments": []any{map[string]any{"type": "photo", "photo": map[string]any{"sizes": []any{map[string]any{"url": server.URL + "/file", "width": 100, "height": 100}}}}},
					}
					switch testCase.refreshChange {
					case "peer":
						message["peer_id"] = 2000000002
					case "sender":
						message["from_id"] = 73
					case "empty":
						message["attachments"] = []any{}
					case "type":
						message["attachments"] = []any{map[string]any{"type": "doc", "doc": map[string]any{"url": server.URL + "/file", "title": "file"}}}
					}
					if err := json.NewEncoder(writer).Encode(map[string]any{"response": map[string]any{"items": []any{message}}}); err != nil {
						t.Error(err)
					}
				default:
					sends++
					t.Error("unexpected request after failed media download")
					writer.WriteHeader(400)
				}
			}))
			defer server.Close()
			config := Config{VKAccessToken: "fake-token", VKTargetPeerID: 2000000001, VKBlockedSenderIDs: map[int64]struct{}{73: {}}}
			vk, err := NewVKClient(config, server.URL, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			clock := &fakeVKClock{now: time.Unix(1_900_000_000, 0)}
			vk.rate.clock, vk.rate.minimum = clock, vkAPIMinInterval
			store := testMessageStore(t)
			if err := vk.ConfigureRateProtection(ctx, store, slog.New(slog.DiscardHandler)); err != nil {
				t.Fatal(err)
			}
			logger := slog.New(slog.DiscardHandler)
			if testCase.cancel {
				logger = slog.New(slog.NewTextHandler(cancelMediaRetryWriter{cancel: cancel}, nil))
			}
			directory := t.TempDir()
			relay := Relay{config: config, vk: vk, store: store, mediaHTTP: server.Client(), tempRoot: directory, logger: logger, retryDelay: time.Millisecond}
			err = relay.deliverMapped(ctx, RenderedMessage{VKMessageID: 99, Name: "Sender", Text: "text", Media: []MediaSource{{Kind: "photo", URL: server.URL + "/file", Name: "photo.jpg"}}})
			if err == nil || downloads != testCase.downloads || refresh != testCase.refresh || sends != 0 {
				t.Fatalf("downloads=%d refresh=%d sends=%d err=%v", downloads, refresh, sends, err)
			}
			if testCase.cancel && !errors.Is(err, context.Canceled) {
				t.Fatalf("expected cancellation: %v", err)
			}
			if testCase.vkCode != 0 {
				var apiError *VKAPIError
				if !errors.As(err, &apiError) || apiError.Code != testCase.vkCode {
					t.Fatalf("flood error not preserved: %v", err)
				}
				until, _, err := store.LoadVKRateState(t.Context())
				if err != nil || !until.After(clock.Now()) {
					t.Fatal("refresh bypassed persisted VK cooldown")
				}
			}
			if testCase.refresh > 1 && len(clock.waits) == 0 {
				t.Fatal("refresh calls bypassed VK pacing")
			}
			mapping, err := store.Lookup(t.Context(), 99)
			if err != nil || mapping != 0 {
				t.Fatal("failed media saved a mapping")
			}
			entries, err := os.ReadDir(directory)
			if err != nil || len(entries) != 0 {
				t.Fatal("media retries leaked temporary files")
			}
		})
	}
}

func TestMediaRefreshAlbum(t *testing.T) {
	downloads := make(map[string]int)
	refresh, sends := 0, 0
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/first", "/expired", "/fresh":
			downloads[request.URL.Path]++
			if request.URL.Path == "/expired" {
				writer.WriteHeader(404)
				return
			}
			writeFixture(t, writer, "photo-bytes")
		case "/messages.getById":
			refresh++
			if sends != 0 {
				t.Error("album was partially sent before media refresh")
			}
			writeFixture(t, writer, fmt.Sprintf(`{"response":{"items":[{"id":99,"peer_id":2000000001,"from_id":42,"out":1,"text":"edited text","attachments":[{"type":"photo","photo":{"sizes":[{"url":%q,"width":100,"height":100}]}},{"type":"photo","photo":{"sizes":[{"url":%q,"width":100,"height":100}]}}]}]}}`, server.URL+"/first", server.URL+"/fresh"))
		case "/botfake-token/sendMediaGroup":
			sends++
			if err := request.ParseMultipartForm(1 << 20); err != nil {
				t.Error(err)
				return
			}
			defer request.MultipartForm.RemoveAll()
			var reply TelegramReplyParameters
			var album []struct {
				Caption string `json:"caption"`
			}
			if json.Unmarshal([]byte(request.FormValue("reply_parameters")), &reply) != nil || reply.MessageID != 77 {
				t.Error("album retry lost original reply")
			}
			if json.Unmarshal([]byte(request.FormValue("media")), &album) != nil || len(album) != 2 || !strings.Contains(album[0].Caption, "original text") || strings.Contains(album[0].Caption, "edited text") {
				t.Error("album retry lost original caption or media")
			}
			writeFixture(t, writer, `{"ok":true,"result":[{"message_id":201},{"message_id":202}]}`)
		default:
			t.Error("unexpected request during album retry")
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
	store := testMessageStore(t)
	if err := store.Save(t.Context(), 98, 77); err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	relay := Relay{config: config, vk: vk, telegram: telegram, store: store, mediaHTTP: server.Client(), tempRoot: directory, logger: slog.New(slog.DiscardHandler), retryDelay: time.Millisecond}
	message := RenderedMessage{VKMessageID: 99, VKReplyToID: 98, Name: "Sender", Text: "original text", Media: []MediaSource{
		{Kind: "photo", URL: server.URL + "/first", Name: "first.jpg"},
		{Kind: "photo", URL: server.URL + "/expired", Name: "second.jpg"},
	}}
	if err := relay.deliverMapped(t.Context(), message); err != nil {
		t.Fatal(err)
	}
	if refresh != 1 || sends != 1 || downloads["/first"] != 2 || downloads["/expired"] != 1 || downloads["/fresh"] != 1 {
		t.Fatalf("refresh=%d sends=%d downloads=%v", refresh, sends, downloads)
	}
	mapping, err := store.Lookup(t.Context(), 99)
	if err != nil || mapping != 201 {
		t.Fatal("album retry did not save its canonical mapping")
	}
	entries, err := os.ReadDir(directory)
	if err != nil || len(entries) != 0 {
		t.Fatal("album retry leaked temporary files")
	}
}

func TestLargeAlbum(t *testing.T) {
	mediaServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) { writeFixture(t, writer, "photo") }))
	defer mediaServer.Close()
	groups, photos := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.ParseMultipartForm(1<<20) != nil {
			t.Error("invalid multipart body")
			return
		}
		defer request.MultipartForm.RemoveAll()
		if strings.HasSuffix(request.URL.Path, "sendMediaGroup") {
			groups++
			var items []struct {
				Caption string `json:"caption"`
			}
			if json.Unmarshal([]byte(request.FormValue("media")), &items) != nil || len(items) != 10 {
				t.Error("incorrect album batch size")
			}
			for index, item := range items {
				if strings.Contains(item.Caption, relayFooter) || (index > 0 && item.Caption != "") {
					t.Error("unexpected album caption")
				}
			}
		} else if strings.HasSuffix(request.URL.Path, "sendPhoto") {
			photos++
			if request.FormValue("caption") != "" {
				t.Error("unexpected overflow caption")
			}
		} else {
			t.Error("unexpected method")
		}
		writeTelegramSuccess(t, writer, request)
	}))
	defer server.Close()
	client, err := newTestTelegramClient(Config{TelegramBotToken: "fake-token", TelegramTargetChatID: -123}, server.URL, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	message := RenderedMessage{Name: "Sender", Text: "album"}
	for range 11 {
		message.Media = append(message.Media, MediaSource{Kind: "photo", URL: mediaServer.URL, Name: "photo.jpg"})
	}
	if _, err := deliverMessage(t.Context(), client, mediaServer.Client(), t.TempDir(), message); err != nil {
		t.Fatal(err)
	}
	if groups != 1 || photos != 1 {
		t.Fatal("incorrect album batching")
	}
}

func TestStickerFilenameFromSignature(t *testing.T) {
	for _, test := range []struct {
		name string
		body []byte
		want string
	}{
		{"PNG", transparentStickerPNG(t), "sticker.png"},
		{"WebP", []byte("RIFF\x00\x00\x00\x00WEBPVP8L"), "sticker.webp"},
		{"GIF", []byte("GIF89a"), "sticker.gif"},
		{"JPEG", []byte{0xff, 0xd8, 0xff}, "sticker.jpg"},
		{"not an image", []byte("<html>error</html>"), ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "opaque-download-name")
			if err := os.WriteFile(file, test.body, 0600); err != nil {
				t.Fatal(err)
			}
			name, err := stickerFilename(file)
			if name != test.want || (err != nil) != (test.want == "") {
				t.Fatalf("name=%s err=%v", name, err)
			}
		})
	}
}
