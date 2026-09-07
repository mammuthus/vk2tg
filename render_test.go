package main

import (
	"encoding/json"
	"html"
	"strings"
	"testing"
)

func TestRenderTextAndLimits(t *testing.T) {
	chunks := renderChunks("A <B>", false, "hello <&>\nworld", 4096)
	if len(chunks) != 1 || chunks[0] != "<b>A &lt;B&gt;</b>:\n\nhello &lt;&amp;&gt;\nworld\n\nотправлено через vk2tg" {
		t.Fatalf("unexpected render: %q", chunks)
	}
	body := strings.Repeat("<&😀\n", 1600)
	chunks = renderChunks("A", true, body, 1024)
	var recovered strings.Builder
	for index, chunk := range chunks {
		limit := 4096
		if index == 0 {
			limit = 1024
		}
		plain := html.UnescapeString(strings.ReplaceAll(strings.ReplaceAll(chunk, "<b>", ""), "</b>", ""))
		if utf16Length(plain) > limit || !strings.HasSuffix(plain, "\n"+relayFooter) || strings.Count(plain, relayFooter) != 1 {
			t.Fatal("invalid limit or footer")
		}
		content := strings.TrimSuffix(plain, "\n\n"+relayFooter)
		if index == 0 {
			content = strings.TrimPrefix(content, "A (репост):\n\n")
		}
		recovered.WriteString(content)
	}
	if recovered.String() != body {
		t.Fatal("splitting lost or changed text")
	}
}

func TestNormalizeWall(t *testing.T) {
	message := VKMessage{Attachments: []VKAttachment{{Type: "wall", Wall: &VKWall{OwnerID: -42, ID: 7, Text: "line <one>\nline two", Attachments: []VKAttachment{{Type: "doc", Doc: &VKDoc{URL: "https://example.test/file", Title: "file.txt"}}}}}}}
	rendered, err := normalizeMessage(message, "Sender")
	if err != nil {
		t.Fatal(err)
	}
	if !rendered.Repost || !strings.Contains(rendered.Text, "line <one>\nline two") || !strings.Contains(rendered.Text, "https://vk.com/wall-42_7") || len(rendered.Media) != 1 {
		t.Fatal("wall content missing")
	}
	message.Attachments[0].Wall = &VKWall{}
	rendered, err = normalizeMessage(message, "Sender")
	if err != nil || !strings.Contains(rendered.Text, "📰 Запись на стене") {
		t.Fatal("missing empty-wall fallback")
	}
}

func TestNestedWall(t *testing.T) {
	var message VKMessage
	if err := json.Unmarshal([]byte(`{"attachments":[{"type":"wall","wall":{"copy_history":[{"owner_id":-42,"id":9,"text":"original\n<&>","attachments":[{"type":"photo","photo":{"sizes":[{"url":"https://example.test/photo","width":100,"height":100}]}}]}]}}]}`), &message); err != nil {
		t.Fatal(err)
	}
	rendered, err := normalizeMessage(message, "Sender")
	if err != nil || !strings.Contains(rendered.Text, "original\n<&>") || strings.Contains(rendered.Text, "📰 Запись на стене") || len(rendered.Media) != 1 {
		t.Fatalf("nested wall lost content: %v", err)
	}
	caption := renderChunks(rendered.Name, rendered.Repost, rendered.Text, 1024)[0]
	if !strings.Contains(caption, "original\n&lt;&amp;&gt;") || !strings.HasSuffix(caption, relayFooter) {
		t.Fatal("nested wall rendered incorrectly")
	}
}
