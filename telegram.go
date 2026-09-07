package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type TelegramClient struct {
	endpoint   string
	chatID     int64
	httpClient *http.Client
	logger     *slog.Logger
}

type TelegramError struct {
	HTTPStatus int
	Code       int
	RetryAfter time.Duration
}

func (failure *TelegramError) Error() string {
	return fmt.Sprintf("telegram send rejected: HTTP %d, API code %d", failure.HTTPStatus, failure.Code)
}

func NewTelegramClient(config Config, baseURL string, timeout time.Duration) (*TelegramClient, error) {
	if config.TelegramBotToken == "" || strings.ContainsAny(config.TelegramBotToken, "/?# \r\n") || config.TelegramTargetChatID == 0 || timeout <= 0 {
		return nil, errors.New("invalid telegram client configuration")
	}
	if baseURL == "" {
		baseURL = "https://api.telegram.org"
	}
	endpoint, err := url.Parse(baseURL)
	if err != nil || endpoint.Host == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" || (endpoint.Scheme != "https" && endpoint.Scheme != "http") {
		return nil, errors.New("invalid telegram endpoint")
	}
	return &TelegramClient{
		endpoint:   strings.TrimRight(baseURL, "/") + "/bot" + config.TelegramBotToken,
		chatID:     config.TelegramTargetChatID,
		httpClient: &http.Client{Timeout: timeout, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }},
	}, nil
}

type TelegramReplyParameters struct {
	MessageID                int64 `json:"message_id"`
	AllowSendingWithoutReply bool  `json:"allow_sending_without_reply"`
}

func telegramReply(identifier int64) *TelegramReplyParameters {
	if identifier <= 0 {
		return nil
	}
	return &TelegramReplyParameters{MessageID: identifier, AllowSendingWithoutReply: true}
}

func (client *TelegramClient) SendMessage(ctx context.Context, text string, replyID int64) (int64, error) {
	body, err := json.Marshal(struct {
		ChatID    int64                    `json:"chat_id"`
		Text      string                   `json:"text"`
		ParseMode string                   `json:"parse_mode"`
		Reply     *TelegramReplyParameters `json:"reply_parameters,omitempty"`
	}{client.chatID, text, "HTML", telegramReply(replyID)})
	if err != nil {
		return 0, errors.New("telegram request encoding failed")
	}
	return client.send(ctx, "sendMessage", "application/json", bytes.NewReader(body))
}

func (client *TelegramClient) send(ctx context.Context, method, contentType string, body io.ReadSeeker) (int64, error) {
	for {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		if _, err := body.Seek(0, io.SeekStart); err != nil {
			return 0, errors.New("telegram request rewind failed")
		}
		identifier, err := client.sendOnce(ctx, method, contentType, body)
		var failure *TelegramError
		if !errors.As(err, &failure) || (failure.Code != 429 && failure.HTTPStatus != 429) {
			return identifier, err
		}
		delay := failure.RetryAfter
		if delay <= 0 {
			delay = time.Second
		}
		if err := pause(ctx, delay); err != nil {
			return 0, err
		}
	}
}

func (client *TelegramClient) sendOnce(ctx context.Context, method, contentType string, body io.ReadSeeker) (int64, error) {
	length, err := body.Seek(0, io.SeekEnd)
	if err != nil {
		return 0, errors.New("telegram request length failed")
	}
	if _, err := body.Seek(0, io.SeekStart); err != nil {
		return 0, errors.New("telegram request rewind failed")
	}
	source, ok := body.(io.ReaderAt)
	if !ok {
		return 0, errors.New("telegram request body is not replayable")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, client.endpoint+"/"+method, io.NewSectionReader(source, 0, length))
	if err != nil {
		return 0, errors.New("telegram request creation failed")
	}
	request.ContentLength = length
	request.Header.Set("Content-Type", contentType)
	response, err := client.httpClient.Do(request)
	if err != nil {
		return 0, safeTransferError(ctx, "telegram transport failed", err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil {
		return 0, safeTransferError(ctx, "telegram response read failed", err)
	}
	var envelope struct {
		Result     json.RawMessage `json:"result"`
		OK         bool            `json:"ok"`
		Code       int             `json:"error_code"`
		Parameters struct {
			RetryAfter int64 `json:"retry_after"`
		} `json:"parameters"`
	}
	decodeErr := json.Unmarshal(data, &envelope)
	if response.StatusCode != http.StatusOK || (decodeErr == nil && !envelope.OK) {
		failure := &TelegramError{HTTPStatus: response.StatusCode, Code: envelope.Code}
		if envelope.Parameters.RetryAfter > 0 && envelope.Parameters.RetryAfter <= int64((1<<63-1)/time.Second) {
			failure.RetryAfter = time.Duration(envelope.Parameters.RetryAfter) * time.Second
		}
		return 0, failure
	}
	if len(data) > 1<<20 || decodeErr != nil {
		return 0, errors.New("invalid telegram response")
	}
	type sentMessage struct {
		ID int64 `json:"message_id"`
	}
	var messages []sentMessage
	if method == "sendMediaGroup" {
		if json.Unmarshal(envelope.Result, &messages) != nil || len(messages) == 0 {
			return 0, errors.New("invalid telegram media result")
		}
	} else {
		var message sentMessage
		if json.Unmarshal(envelope.Result, &message) != nil {
			return 0, errors.New("invalid telegram message result")
		}
		messages = []sentMessage{message}
	}
	for _, message := range messages {
		if message.ID <= 0 {
			return 0, errors.New("missing telegram message identifier")
		}
	}
	if client.logger != nil {
		client.logger.Info("telegram send confirmed", "method", method, "message_count", len(messages))
	}
	return messages[0].ID, nil
}

func safeTransferError(ctx context.Context, message string, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%s: %w", message, context.DeadlineExceeded)
	}
	return errors.New(message)
}

func pause(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
