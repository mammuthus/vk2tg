package main

import (
	"encoding/json"
	"html"
	"strings"
	"testing"
)

func TestRenderTextAndLimits(t *testing.T) {
	chunks := renderChunks("A <B>", false, "hello <&>\nworld", 4096, "")
	if len(chunks) != 1 || chunks[0] != "<b>A &lt;B&gt;</b>\n\nhello &lt;&amp;&gt;\nworld\n\nотправлено через vk2tg" {
		t.Fatalf("unexpected render: %q", chunks)
	}
	body := strings.Repeat("<&😀\n", 1600)
	chunks = renderChunks("A", true, body, 1024, "")
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
			content = strings.TrimPrefix(content, "A (репост)\n\n")
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
	if !rendered.Repost || !strings.Contains(rendered.Text, "line <one>\nline two") || rendered.SourceURL != "https://vk.ru/wall-42_7" || strings.Contains(rendered.Text, rendered.SourceURL) || len(rendered.Media) != 1 {
		t.Fatal("wall content missing")
	}
	message.Attachments[0].Wall = &VKWall{}
	rendered, err = normalizeMessage(message, "Sender")
	if err != nil || !strings.Contains(rendered.Text, "📰 Запись на стене") || rendered.WallFallbacks != 1 {
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
	caption := renderChunks(rendered.Name, rendered.Repost, rendered.Text, 1024, rendered.SourceURL)[0]
	if !strings.Contains(caption, "original\n&lt;&amp;&gt;") || !strings.HasSuffix(caption, relayFooter) {
		t.Fatal("nested wall rendered incorrectly")
	}
}

func TestRepostSourceHeader(t *testing.T) {
	for _, test := range []struct {
		name   string
		post   VKWall
		source string
	}{
		{name: "generated", post: VKWall{OwnerID: -42, ID: 7}, source: "https://vk.ru/wall-42_7"},
		{name: "without source", post: VKWall{}},
		{name: "ready URL preserved", post: VKWall{OwnerID: -42, ID: 7, URL: `https://vk.com/wall-42_7?ref="<&>`}, source: `https://vk.com/wall-42_7?ref="<&>`},
	} {
		t.Run(test.name, func(t *testing.T) {
			test.post.Text = "wall <&>\nnext line"
			rendered, err := normalizeMessage(VKMessage{Attachments: []VKAttachment{{Type: "wall", Wall: &test.post}}}, "A <B>")
			if err != nil || rendered.SourceURL != test.source || rendered.Text != test.post.Text {
				t.Fatalf("source selection or wall text changed: %v", err)
			}
			label := "репост"
			if test.source != "" {
				label = `<a href="` + html.EscapeString(test.source) + `">репост</a>`
			}
			chunks := renderChunks(rendered.Name, rendered.Repost, rendered.Text, 1024, rendered.SourceURL)
			want := "<b>A &lt;B&gt;</b> (" + label + ")\n\nwall &lt;&amp;&gt;\nnext line\n\n" + relayFooter
			if len(chunks) != 1 || chunks[0] != want {
				t.Fatal("incorrect header, escaping, content or footer")
			}
			if test.source != "" && strings.Count(chunks[0], html.EscapeString(test.source)) != 1 {
				t.Fatal("duplicate source URL")
			}
		})
	}
}

func TestRepostRetainsOtherSourcesAndUserText(t *testing.T) {
	post := VKWall{OwnerID: -42, ID: 7, Text: "outer", CopyHistory: []VKWall{{OwnerID: -42, ID: 8, Text: "inner"}}}
	message := VKMessage{Text: "user URL https://vk.ru/wall-42_7", Attachments: []VKAttachment{{Type: "wall", Wall: &post}}}
	rendered, err := normalizeMessage(message, "Sender")
	if err != nil || rendered.SourceURL != "https://vk.ru/wall-42_7" || rendered.Text != message.Text+"\n\nouter\n\ninner\n\nhttps://vk.ru/wall-42_8" {
		t.Fatal("lost another wall source or modified user text")
	}
}

func TestLinkedRepostLimits(t *testing.T) {
	text := strings.Repeat("<&😀\n", 1600)
	source := "https://vk.ru/wall-42_7?ref=" + strings.Repeat("a", 500)
	plain := renderChunks("Sender", true, text, 1024, "")
	linked := renderChunks("Sender", true, text, 1024, source)
	if len(plain) != len(linked) {
		t.Fatal("URL markup changed splitting")
	}
	for index, chunk := range linked {
		withoutLink := strings.ReplaceAll(chunk, `<a href="`+html.EscapeString(source)+`">репост</a>`, "репост")
		if withoutLink != plain[index] {
			t.Fatal("visible text, limits or continuation changed")
		}
	}
	ordinary := renderChunks("Sender", false, text, 4096, source)
	if strings.Join(ordinary, "") != strings.Join(renderChunks("Sender", false, text, 4096, ""), "") {
		t.Fatal("source affected an ordinary message")
	}
}
