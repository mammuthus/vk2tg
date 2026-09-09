package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestDeliveryCanonicalAndReply(t *testing.T) {
	for _, kind := range []string{"text", "photo document", "album", "document", "long caption", "late failure"} {
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
				if kind == "late failure" && calls == 2 {
					writer.WriteHeader(500)
					writeFixture(t, writer, `{"ok":false,"error_code":500}`)
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
			client, err := NewTelegramClient(Config{TelegramBotToken: "fake-token", TelegramTargetChatID: -123}, server.URL, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			message := RenderedMessage{Name: "Sender", Text: "text", TelegramReplyID: 77}
			photo := MediaSource{Kind: "photo", URL: server.URL + "/file", Name: "photo.jpg"}
			document := MediaSource{Kind: "document", URL: server.URL + "/file", Name: "doc.txt"}
			switch kind {
			case "photo document", "late failure":
				message.Media = []MediaSource{photo, document}
			case "album":
				message.Media = []MediaSource{photo, photo}
			case "document":
				message.Media = []MediaSource{document}
			case "long caption":
				message.Media = []MediaSource{photo}
				message.Text = strings.Repeat("x", 6000)
			}
			identifier, err := deliverMessage(t.Context(), client, server.Client(), t.TempDir(), message)
			if kind == "late failure" {
				if err == nil || identifier != 0 {
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
			client, err := NewTelegramClient(Config{TelegramBotToken: "fake-token", TelegramTargetChatID: -123}, server.URL, time.Second)
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
	client, err := NewTelegramClient(Config{TelegramBotToken: "fake-token", TelegramTargetChatID: -123}, server.URL, 3*time.Second)
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
	client, err := NewTelegramClient(Config{TelegramBotToken: "fake-token", TelegramTargetChatID: -123}, server.URL, time.Second)
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
