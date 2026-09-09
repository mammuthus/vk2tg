package main

import (
	"bytes"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"testing"
	"time"
)

func transparentStickerPNG(t *testing.T) []byte {
	t.Helper()
	bitmap := image.NewNRGBA(image.Rect(0, 0, 512, 512))
	bitmap.SetNRGBA(256, 256, color.NRGBA{R: 100, G: 50, B: 20, A: 128})
	bitmap.SetNRGBA(255, 255, color.NRGBA{R: 100, G: 50, B: 20, A: 255})
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, bitmap); err != nil {
		t.Fatal(err)
	}
	return encoded.Bytes()
}

func TestStickerPrefersUnbackedImage(t *testing.T) {
	for _, test := range []struct {
		name   string
		images []VKStickerImage
		want   string
	}{
		{"smaller transparent beats background", []VKStickerImage{{URL: "transparent", Width: 256, Height: 256}}, "transparent"},
		{"largest transparent", []VKStickerImage{{URL: "small", Width: 64, Height: 64}, {URL: "large", Width: 512, Height: 512}}, "large"},
		{"invalid transparent fallback", []VKStickerImage{{URL: "invalid", Width: 0, Height: 512}}, "background"},
		{"background only fallback", nil, "background"},
	} {
		t.Run(test.name, func(t *testing.T) {
			sticker := VKSticker{Images: test.images, ImagesWithBackground: []VKStickerImage{{URL: "background", Width: 1024, Height: 1024}}}
			if got := sticker.imageURL(); got != test.want {
				t.Fatalf("got %s want %s", got, test.want)
			}
		})
	}
}

func TestStickerTransparencyThroughDelivery(t *testing.T) {
	stickerBytes := transparentStickerPNG(t)
	photoBytes := []byte("ordinary-photo-unchanged")
	var methods []string
	var captions []string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/transparent":
			writer.Write(stickerBytes)
			return
		case "/photo":
			writer.Write(photoBytes)
			return
		case "/background":
			t.Error("selected background instead of transparent variant")
			writer.WriteHeader(400)
			return
		}
		methods = append(methods, request.URL.Path)
		if err := request.ParseMultipartForm(1 << 20); err != nil {
			t.Error(err)
			return
		}
		defer request.MultipartForm.RemoveAll()
		captions = append(captions, request.FormValue("caption"))
		var reply TelegramReplyParameters
		if err := json.Unmarshal([]byte(request.FormValue("reply_parameters")), &reply); err != nil || reply.MessageID != 77 || !reply.AllowSendingWithoutReply {
			t.Error("sticker/photo reply changed")
		}
		field := "photo"
		if request.URL.Path == "/botfake-token/sendDocument" {
			field = "document"
		}
		file, header, err := request.FormFile(field)
		if err != nil {
			t.Error(err)
			return
		}
		defer file.Close()
		body, err := io.ReadAll(file)
		if err != nil {
			t.Error(err)
			return
		}
		if field == "document" {
			if header.Filename != "sticker.png" || !bytes.Equal(body, stickerBytes) || request.FormValue("disable_content_type_detection") != "true" {
				t.Error("sticker bytes or upload mode changed")
			}
			bitmap, err := png.Decode(bytes.NewReader(body))
			if err != nil {
				t.Error(err)
				return
			}
			if bitmap.Bounds() != image.Rect(0, 0, 512, 512) {
				t.Error("sticker dimensions changed")
			}
			for _, point := range []struct {
				name     string
				location image.Point
				alpha    uint32
			}{
				{"transparent", image.Pt(0, 0), 0},
				{"partial", image.Pt(256, 256), 0x8080},
				{"opaque", image.Pt(255, 255), 0xffff},
			} {
				_, _, _, alpha := bitmap.At(point.location.X, point.location.Y).RGBA()
				if alpha != point.alpha {
					t.Errorf("%s pixel alpha changed: %d", point.name, alpha)
				}
			}
		} else if !bytes.Equal(body, photoBytes) || request.FormValue("disable_content_type_detection") != "" {
			t.Error("ordinary photo changed")
		}
		writeTelegramSuccess(t, writer, request)
	}))
	defer server.Close()
	client, err := newTestTelegramClient(Config{TelegramBotToken: "fake-token", TelegramTargetChatID: -123}, server.URL, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	message := VKMessage{Attachments: []VKAttachment{
		{Type: "sticker", Sticker: &VKSticker{Images: []VKStickerImage{{URL: server.URL + "/transparent", Width: 512, Height: 512}}, ImagesWithBackground: []VKStickerImage{{URL: server.URL + "/background", Width: 1024, Height: 1024}}}},
	}}
	rendered, err := normalizeMessage(message, "Sender")
	if err != nil {
		t.Fatal(err)
	}
	rendered.TelegramReplyID = 77
	rendered.Media = append(rendered.Media, MediaSource{Kind: "photo", URL: server.URL + "/photo", Name: "photo.jpg"})
	directory := t.TempDir()
	canonical, err := deliverMessage(t.Context(), client, server.Client(), directory, rendered)
	if err != nil || canonical != 101 {
		t.Fatalf("delivery failed: id=%d err=%v", canonical, err)
	}
	if !reflect.DeepEqual(methods, []string{"/botfake-token/sendDocument", "/botfake-token/sendPhoto"}) {
		t.Fatalf("unexpected methods: %v", methods)
	}
	if !reflect.DeepEqual(captions, []string{"<b>Sender</b>\n\n", ""}) {
		t.Fatal("caption or header changed")
	}
	entries, err := os.ReadDir(directory)
	if err != nil || len(entries) != 0 {
		t.Fatal("sticker temp files leaked")
	}
}
