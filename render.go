package main

import (
	"fmt"
	"html"
	"strings"
	"unicode/utf8"
)

type MediaSource struct {
	Kind string
	URL  string
	Name string
}

type RenderedMessage struct {
	UnsupportedAttachments int
	WallFallbacks          int
	IgnoredForwards        int
	VKMessageID            int64
	VKReplyToID            int64
	TelegramReplyID        int64
	Name                   string
	Repost                 bool
	SourceURL              string
	Text                   string
	Media                  []MediaSource
}

func normalizeMessage(message VKMessage, name string) (RenderedMessage, error) {
	result := RenderedMessage{Name: name, VKMessageID: message.ID, IgnoredForwards: len(message.ForwardedMessages)}
	if message.ReplyMessage != nil {
		result.VKReplyToID = message.ReplyMessage.ID
	}
	var texts []string
	if message.Text != "" {
		texts = append(texts, message.Text)
	}
	var attachments func([]VKAttachment, int) error
	var wall func(VKWall, int) error
	wall = func(post VKWall, depth int) error {
		if depth > 8 {
			return ErrVKInvalidResponse
		}
		result.Repost = true
		source := post.URL
		if source == "" && post.ID != 0 && post.OwnerID != 0 {
			source = fmt.Sprintf("https://vk.ru/wall%d_%d", post.OwnerID, post.ID)
		}
		if result.SourceURL == "" {
			result.SourceURL = source
		}
		startText, startMedia := len(texts), len(result.Media)
		if post.Text != "" {
			texts = append(texts, post.Text)
		}
		if err := attachments(post.Attachments, depth+1); err != nil {
			return err
		}
		for _, original := range post.CopyHistory {
			if err := wall(original, depth+1); err != nil {
				return err
			}
		}
		if len(texts) == startText && len(result.Media) == startMedia {
			texts = append(texts, "📰 Запись на стене")
			result.WallFallbacks++
		}
		if source != "" && source != result.SourceURL {
			texts = append(texts, source)
		}
		return nil
	}
	attachments = func(items []VKAttachment, depth int) error {
		if depth > 8 {
			return ErrVKInvalidResponse
		}
		for _, attachment := range items {
			switch attachment.Type {
			case "sticker":
				address := attachment.Sticker.imageURL()
				if address == "" {
					return ErrVKInvalidResponse
				}
				result.Media = append(result.Media, MediaSource{Kind: "photo", URL: address, Name: "sticker.png"})
			case "link":
				if attachment.Link == nil || strings.TrimSpace(attachment.Link.URL) == "" {
					return ErrVKInvalidResponse
				}
				if !strings.Contains(strings.Join(texts, "\n\n"), attachment.Link.URL) {
					texts = append(texts, attachment.Link.URL)
				}
			case "photo":
				if attachment.Photo == nil {
					return ErrVKInvalidResponse
				}
				var bestURL string
				var bestArea int64
				for _, size := range attachment.Photo.Sizes {
					if size.URL != "" && (bestURL == "" || size.Width*size.Height > bestArea) {
						bestURL, bestArea = size.URL, size.Width*size.Height
					}
				}
				if bestURL == "" {
					return ErrVKInvalidResponse
				}
				result.Media = append(result.Media, MediaSource{Kind: "photo", URL: bestURL, Name: "photo.jpg"})
			case "doc":
				if attachment.Doc == nil || attachment.Doc.URL == "" {
					return ErrVKInvalidResponse
				}
				result.Media = append(result.Media, MediaSource{Kind: "document", URL: attachment.Doc.URL, Name: attachment.Doc.Title})
			case "wall":
				if attachment.Wall == nil {
					return ErrVKInvalidResponse
				}
				if err := wall(*attachment.Wall, depth+1); err != nil {
					return err
				}
			default:
				texts = append(texts, "[Unsupported attachment]")
				result.UnsupportedAttachments++
			}
		}
		return nil
	}
	if err := attachments(message.Attachments, 0); err != nil {
		return RenderedMessage{}, err
	}
	result.Text = strings.Join(texts, "\n\n")
	return result, nil
}

func renderChunks(name string, repost bool, text string, firstLimit int, sourceURL string) []string {
	name, _ = takeUTF16(name, 256)
	if name == "" {
		name = "VK sender"
	}
	suffix := "\n\n"
	if repost {
		suffix = " (репост)\n\n"
	}
	headerLength := utf16Length(name + suffix)
	if repost && sourceURL != "" {
		suffix = " (<a href=\"" + html.EscapeString(sourceURL) + "\">репост</a>)\n\n"
	}
	header := "<b>" + html.EscapeString(name) + "</b>" + suffix
	var chunks []string
	limit := firstLimit
	for {
		part, remaining := takeUTF16(text, limit-headerLength)
		chunks = append(chunks, header+html.EscapeString(part))
		if remaining == "" {
			return chunks
		}
		text = remaining
		header, headerLength, limit = "", 0, 4096
	}
}

func utf16Length(text string) int {
	length := 0
	for _, character := range text {
		length++
		if character > 0xffff {
			length++
		}
	}
	return length
}

func takeUTF16(text string, limit int) (string, string) {
	position, units := 0, 0
	for position < len(text) {
		character, size := utf8.DecodeRuneInString(text[position:])
		cost := 1
		if character > 0xffff {
			cost = 2
		}
		if units+cost > limit {
			break
		}
		position += size
		units += cost
	}
	return text[:position], text[position:]
}
