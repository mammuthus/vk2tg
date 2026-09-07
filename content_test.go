package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestLinkAttachments(t *testing.T) {
	for _, text := range []string{"See https://example.test/lesson", ""} {
		t.Run(text, func(t *testing.T) {
			var message VKMessage
			if err := json.Unmarshal([]byte(`{"attachments":[{"type":"link","link":{"url":"https://example.test/lesson","title":"Lesson"}}]}`), &message); err != nil {
				t.Fatal(err)
			}
			message.Text = text
			rendered, err := normalizeMessage(message, "Sender")
			if err != nil || strings.Count(rendered.Text, "https://example.test/lesson") != 1 || rendered.UnsupportedAttachments != 0 || strings.Contains(rendered.Text, "[Unsupported attachment]") {
				t.Fatalf("link normalization failed: %v", err)
			}
		})
	}
}

func TestStickerImages(t *testing.T) {
	var message VKMessage
	if err := json.Unmarshal([]byte(`{"attachments":[{"type":"sticker","sticker":{"inner_type":"base_sticker","sticker_id":42,"product_id":7,"is_allowed":true,"images":[{"url":"https://example.test/64.png","width":64,"height":64},{"url":"https://example.test/512.png","width":512,"height":512}],"images_with_background":[{"url":"https://example.test/background.png","width":512,"height":512}]}}]}`), &message); err != nil {
		t.Fatal(err)
	}
	rendered, err := normalizeMessage(message, "Sender")
	if err != nil || len(rendered.Media) != 1 || rendered.Media[0].Kind != "photo" || rendered.Media[0].URL != "https://example.test/background.png" || rendered.UnsupportedAttachments != 0 {
		t.Fatalf("sticker selection failed: %v", err)
	}
	message.Attachments[0].Sticker.ImagesWithBackground = nil
	rendered, err = normalizeMessage(message, "Sender")
	if err != nil || rendered.Media[0].URL != "https://example.test/512.png" {
		t.Fatal("largest transparent variant missing")
	}
}

func TestLongPollServiceContent(t *testing.T) {
	for _, test := range []struct {
		event    string
		retained bool
	}{
		{`[4,1,0,2000000001,0,"",{"from":"42","source_act":"chat_pin_message"},{}]`, false},
		{`[4,1,0,2000000001,0,"useful",{"from":"42","source_act":"chat_pin_message"},{}]`, true},
		{`[4,1,0,2000000001,0,"",{"from":"42","source_act":"chat_pin_message"},{"attach1_type":"photo"}]`, true},
	} {
		message, full, err := decodeLongPollMessage(json.RawMessage(test.event))
		if err != nil || (message.ID != 0) != test.retained || full != test.retained {
			t.Fatal("service content filtering or enrichment regression")
		}
	}
}

func TestServiceContent(t *testing.T) {
	for _, test := range []struct {
		name, body string
		accepted   bool
	}{
		{"pin", `{"action":{"type":"chat_pin_message","member_id":42,"conversation_message_id":7},"text":"","attachments":[]}`, false},
		{"empty", `{"text":" \n "}`, false},
		{"pin text", `{"action":{"type":"chat_pin_message"},"text":"useful"}`, true},
		{"pin media", `{"action":{"type":"chat_pin_message"},"attachments":[{"type":"photo","photo":{"sizes":[{"url":"https://example.test/photo","width":100,"height":100}]}}]}`, true},
		{"unknown", `{"attachments":[{"type":"audio"}]}`, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var message VKMessage
			if err := json.Unmarshal([]byte(test.body), &message); err != nil {
				t.Fatal(err)
			}
			message.FromID = 42
			relay := Relay{senderNames: map[int64]string{42: "Sender"}}
			rendered, accepted, err := relay.prepareMessage(t.Context(), message)
			if err != nil || accepted != test.accepted {
				t.Fatalf("content filtering failed: %v", err)
			}
			if test.name == "pin" && (message.Action == nil || message.Action.Type != "chat_pin_message") {
				t.Fatal("lost real action type")
			}
			if test.name == "unknown" && rendered.Text != "[Unsupported attachment]" {
				t.Fatal("unknown attachment behavior changed")
			}
		})
	}
}
