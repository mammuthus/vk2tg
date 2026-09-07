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

func deliverMessage(ctx context.Context, telegram *TelegramClient, mediaHTTP *http.Client, tempRoot string, message RenderedMessage) (result error) {
	limit := 4096
	if len(message.Media) > 0 {
		limit = 1024
	}
	chunks := renderChunks(message.Name, message.Repost, message.Text, limit)
	if len(message.Media) == 0 {
		for _, chunk := range chunks {
			if err := telegram.SendMessage(ctx, chunk); err != nil {
				return err
			}
		}
		return nil
	}
	directory, err := os.MkdirTemp(tempRoot, "vk2tg-media-*")
	if err != nil {
		return errors.New("cannot create media directory")
	}
	defer func() {
		if os.RemoveAll(directory) != nil {
			result = errors.Join(result, errors.New("temporary media cleanup failed"))
		}
	}()
	files := make([]localMedia, 0, len(message.Media))
	for _, source := range message.Media {
		path, err := downloadMedia(ctx, mediaHTTP, directory, source)
		if err != nil {
			return err
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
		caption := "\n" + relayFooter
		if first {
			caption = chunks[0]
		}
		if err := telegram.sendFiles(ctx, directory, files[position:end], caption); err != nil {
			return err
		}
		if first {
			for _, chunk := range chunks[1:] {
				if err := telegram.SendMessage(ctx, chunk); err != nil {
					return err
				}
			}
		}
		first = false
		position = end
	}
	return nil
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

func (client *TelegramClient) sendFiles(ctx context.Context, directory string, files []localMedia, caption string) error {
	body, err := os.CreateTemp(directory, "telegram-upload-*")
	if err != nil {
		return errors.New("cannot create upload body")
	}
	defer body.Close()
	defer os.Remove(body.Name())
	writer := multipart.NewWriter(body)
	if writer.WriteField("chat_id", strconv.FormatInt(client.chatID, 10)) != nil {
		return errors.New("cannot encode upload")
	}
	method := "sendPhoto"
	if len(files) > 1 {
		method = "sendMediaGroup"
	} else if files[0].Kind == "document" {
		method = "sendDocument"
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
		if len(files) > 1 {
			field = "media" + strconv.Itoa(index)
			itemCaption := "\n" + relayFooter
			if index == 0 {
				itemCaption = caption
			}
			album = append(album, albumItem{Type: "photo", Media: "attach://" + field, Caption: itemCaption, ParseMode: "HTML"})
		}
		part, err := writer.CreateFormFile(field, safeFilename(file.Name))
		if err != nil {
			return errors.New("cannot encode upload file")
		}
		input, err := os.Open(file.Path)
		if err != nil {
			return errors.New("cannot open upload file")
		}
		_, copyErr := io.Copy(part, input)
		closeErr := input.Close()
		if copyErr != nil || closeErr != nil {
			return errors.New("cannot prepare upload file")
		}
	}
	if len(album) > 0 {
		encoded, err := json.Marshal(album)
		if err != nil || writer.WriteField("media", string(encoded)) != nil {
			return errors.New("cannot encode media group")
		}
	} else {
		if writer.WriteField("caption", caption) != nil || writer.WriteField("parse_mode", "HTML") != nil {
			return errors.New("cannot encode caption")
		}
	}
	if writer.Close() != nil {
		return errors.New("cannot finalize upload")
	}
	return client.send(ctx, method, writer.FormDataContentType(), body)
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
