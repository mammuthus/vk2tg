package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type localMedia struct {
	MediaSource
	Path string
}

func deliverMessage(ctx context.Context, telegram *TelegramClient, mediaHTTP *http.Client, tempRoot string, message RenderedMessage) (canonical int64, result error) {
	limit := 4096
	if len(message.Media) > 0 {
		limit = 1024
	}
	chunks := renderChunks(message.Name, message.Repost, message.Text, limit, message.SourceURL)
	if len(message.Media) == 0 {
		for _, chunk := range chunks {
			identifier, err := telegram.SendMessage(ctx, chunk, message.TelegramReplyID)
			if err != nil {
				return 0, err
			}
			if canonical == 0 {
				canonical = identifier
			}
		}
		return canonical, nil
	}
	directory, err := os.MkdirTemp(tempRoot, "vk2tg-media-*")
	if err != nil {
		return 0, errors.New("cannot create media directory")
	}
	defer func() {
		if os.RemoveAll(directory) != nil {
			result = errors.Join(result, errors.New("temporary media cleanup failed"))
			canonical = 0
		}
	}()
	files := make([]localMedia, 0, len(message.Media))
	for _, source := range message.Media {
		path, err := downloadMedia(ctx, mediaHTTP, directory, source)
		if err != nil {
			return 0, err
		}
		if source.Kind == "sticker" {
			source.Name, err = stickerFilename(path)
			if err != nil {
				return 0, err
			}
		}
		files = append(files, localMedia{MediaSource: source, Path: path})
	}
	first := true
	for position := 0; position < len(files); {
		end := position + 1
		if files[position].Kind == "photo" {
			for end < len(files) && files[end].Kind == "photo" && end-position < 10 {
				end++
			}
		}
		caption := ""
		if first {
			caption = chunks[0]
		}
		identifier, err := telegram.sendFiles(ctx, directory, files[position:end], caption, message.TelegramReplyID)
		if err != nil {
			return 0, err
		}
		if first {
			canonical = identifier
			for _, chunk := range chunks[1:] {
				if _, err := telegram.SendMessage(ctx, chunk, message.TelegramReplyID); err != nil {
					return 0, err
				}
			}
		}
		first = false
		position = end
	}
	return canonical, nil
}

func downloadMedia(ctx context.Context, client *http.Client, directory string, source MediaSource) (string, error) {
	endpoint, err := url.Parse(source.URL)
	if err != nil || endpoint.Host == "" || endpoint.User != nil || (endpoint.Scheme != "https" && endpoint.Scheme != "http") {
		return "", errors.New("invalid media URL")
	}
	file, err := os.CreateTemp(directory, "vk-media-*")
	if err != nil {
		return "", errors.New("cannot create media file")
	}
	path := file.Name()
	if file.Close() != nil {
		return "", errors.New("cannot close media file")
	}
	maximum := int64(10_000_000)
	if source.Kind == "document" {
		maximum = 50_000_000
	}
	for {
		retry, err := downloadAttempt(ctx, client, source.URL, path, maximum)
		if !retry {
			return path, err
		}
		if err := pause(ctx, 2*time.Second); err != nil {
			return "", err
		}
	}
}

func downloadAttempt(ctx context.Context, client *http.Client, address, path string, maximum int64) (bool, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return false, errors.New("media request creation failed")
	}
	response, err := client.Do(request)
	if err != nil {
		return ctx.Err() == nil, safeTransferError(ctx, "media download failed", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return response.StatusCode == 429 || response.StatusCode >= 500, fmt.Errorf("media HTTP status %d", response.StatusCode)
	}
	if response.ContentLength > maximum {
		return false, errors.New("media exceeds upload size limit")
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return false, errors.New("cannot open media file")
	}
	count, copyErr := io.Copy(file, io.LimitReader(response.Body, maximum+1))
	closeErr := file.Close()
	if closeErr != nil {
		return false, errors.New("cannot close downloaded media")
	}
	if copyErr != nil {
		var diskError *os.PathError
		return ctx.Err() == nil && !errors.As(copyErr, &diskError), safeTransferError(ctx, "media transfer failed", copyErr)
	}
	if count == 0 || count > maximum {
		return false, errors.New("empty or oversized media")
	}
	return false, nil
}

func (client *TelegramClient) sendFiles(ctx context.Context, directory string, files []localMedia, caption string, replyID int64) (int64, error) {
	body, err := os.CreateTemp(directory, "telegram-upload-*")
	if err != nil {
		return 0, errors.New("cannot create upload body")
	}
	defer body.Close()
	defer os.Remove(body.Name())
	writer := multipart.NewWriter(body)
	if writer.WriteField("chat_id", strconv.FormatInt(client.chatID, 10)) != nil {
		return 0, errors.New("cannot encode upload")
	}
	if reply := telegramReply(replyID); reply != nil {
		encoded, err := json.Marshal(reply)
		if err != nil || writer.WriteField("reply_parameters", string(encoded)) != nil {
			return 0, errors.New("cannot encode media reply")
		}
	}
	method := "sendPhoto"
	if len(files) > 1 {
		method = "sendMediaGroup"
	} else if files[0].Kind == "document" || files[0].Kind == "sticker" {
		method = "sendDocument"
	}
	if files[0].Kind == "sticker" && writer.WriteField("disable_content_type_detection", "true") != nil {
		return 0, errors.New("cannot encode sticker upload")
	}
	type albumItem struct {
		Type      string `json:"type"`
		Media     string `json:"media"`
		Caption   string `json:"caption"`
		ParseMode string `json:"parse_mode"`
	}
	var album []albumItem
	for index, file := range files {
		field := file.Kind
		if field == "sticker" {
			field = "document"
		}
		if len(files) > 1 {
			field = "media" + strconv.Itoa(index)
			itemCaption := ""
			if index == 0 {
				itemCaption = caption
			}
			album = append(album, albumItem{Type: "photo", Media: "attach://" + field, Caption: itemCaption, ParseMode: "HTML"})
		}
		part, err := writer.CreateFormFile(field, safeFilename(file.Name))
		if err != nil {
			return 0, errors.New("cannot encode upload file")
		}
		input, err := os.Open(file.Path)
		if err != nil {
			return 0, errors.New("cannot open upload file")
		}
		_, copyErr := io.Copy(part, input)
		closeErr := input.Close()
		if copyErr != nil || closeErr != nil {
			return 0, errors.New("cannot prepare upload file")
		}
	}
	if len(album) > 0 {
		encoded, err := json.Marshal(album)
		if err != nil || writer.WriteField("media", string(encoded)) != nil {
			return 0, errors.New("cannot encode media group")
		}
	} else {
		if writer.WriteField("caption", caption) != nil || writer.WriteField("parse_mode", "HTML") != nil {
			return 0, errors.New("cannot encode caption")
		}
	}
	if writer.Close() != nil {
		return 0, errors.New("cannot finalize upload")
	}
	return client.sendWithCost(ctx, method, writer.FormDataContentType(), body, len(files))
}

func stickerFilename(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", errors.New("cannot inspect sticker image")
	}
	defer file.Close()
	var header [512]byte
	count, err := io.ReadFull(file, header[:])
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return "", errors.New("cannot read sticker image header")
	}
	switch http.DetectContentType(header[:count]) {
	case "image/png":
		return "sticker.png", nil
	case "image/webp":
		return "sticker.webp", nil
	case "image/gif":
		return "sticker.gif", nil
	case "image/jpeg":
		return "sticker.jpg", nil
	default:
		return "", errors.New("unsupported sticker image format")
	}
}

func safeFilename(name string) string {
	name = filepath.Base(strings.ReplaceAll(name, "\\", "/"))
	name = strings.Map(func(character rune) rune {
		if character < 32 || character == 127 || character == '"' {
			return '_'
		}
		return character
	}, name)
	name, _ = takeUTF16(name, 200)
	if name == "" || name == "." || name == "/" {
		return "attachment"
	}
	return name
}
