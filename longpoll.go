package main

import (
	"context"
	"encoding/json"
	"html"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

type VKMessage struct {
	ID           int64          `json:"id"`
	PeerID       int64          `json:"peer_id"`
	FromID       int64          `json:"from_id"`
	Out          int            `json:"out"`
	Text         string         `json:"text"`
	Attachments  []VKAttachment `json:"attachments"`
	ReplyMessage *struct {
		ID int64 `json:"id"`
	} `json:"reply_message"`
	ForwardedMessages []struct{} `json:"fwd_messages"`
	Action            *struct {
		Type string `json:"type"`
	} `json:"action"`
}

type VKAttachment struct {
	Sticker *VKSticker `json:"sticker"`
	Type    string     `json:"type"`
	Photo   *VKPhoto   `json:"photo"`
	Doc     *VKDoc     `json:"doc"`
	Wall    *VKWall    `json:"wall"`
	Link    *struct {
		URL   string `json:"url"`
		Title string `json:"title"`
	} `json:"link"`
}

type VKPhoto struct {
	Sizes []struct {
		URL    string `json:"url"`
		Width  int64  `json:"width"`
		Height int64  `json:"height"`
	} `json:"sizes"`
}

type VKDoc struct {
	URL   string `json:"url"`
	Title string `json:"title"`
}

type VKWall struct {
	URL         string         `json:"url"`
	ID          int64          `json:"id"`
	OwnerID     int64          `json:"owner_id"`
	Text        string         `json:"text"`
	Attachments []VKAttachment `json:"attachments"`
	CopyHistory []VKWall       `json:"copy_history"`
}

type VKLongPollBatch struct {
	TS      json.Number       `json:"ts"`
	Failed  int               `json:"failed"`
	Updates []json.RawMessage `json:"updates"`
}

func (client *VKClient) WaitLongPoll(ctx context.Context, server VKLongPollServer) (VKLongPollBatch, error) {
	address := server.Server
	if !strings.Contains(address, "://") {
		address = "https://" + address
	}
	endpoint, err := url.Parse(address)
	if err != nil || endpoint.Host == "" || endpoint.User != nil || (endpoint.Scheme != "https" && endpoint.Scheme != "http") {
		return VKLongPollBatch{}, ErrVKInvalidResponse
	}
	query := endpoint.Query()
	query.Set("act", "a_check")
	query.Set("key", server.Key)
	query.Set("ts", server.TS.String())
	query.Set("wait", "25")
	query.Set("mode", "2")
	query.Set("version", "3")
	endpoint.RawQuery = query.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return VKLongPollBatch{}, ErrVKInvalidResponse
	}
	response, err := client.httpClient.Do(request)
	if err != nil {
		return VKLongPollBatch{}, vkTransportError(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return VKLongPollBatch{}, &VKHTTPError{StatusCode: response.StatusCode}
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil {
		return VKLongPollBatch{}, vkTransportError(err)
	}
	if len(body) > 1<<20 {
		return VKLongPollBatch{}, ErrVKInvalidResponse
	}
	if !json.Valid(body) {
		return VKLongPollBatch{}, ErrVKInvalidJSON
	}
	var batch VKLongPollBatch
	if json.Unmarshal(body, &batch) != nil || ((batch.Failed == 0 || batch.Failed == 1) && batch.TS == "") {
		return VKLongPollBatch{}, ErrVKInvalidResponse
	}
	return batch, nil
}

func (client *VKClient) GetMessageByID(ctx context.Context, identifier int64) (VKMessage, error) {
	var response struct {
		Items []VKMessage `json:"items"`
	}
	if err := client.call(ctx, "messages.getById", url.Values{"message_ids": {strconv.FormatInt(identifier, 10)}}, &response); err != nil {
		return VKMessage{}, err
	}
	if len(response.Items) != 1 || response.Items[0].ID != identifier {
		return VKMessage{}, ErrVKInvalidResponse
	}
	return response.Items[0], nil
}

func decodeLongPollMessage(raw json.RawMessage) (VKMessage, bool, error) {
	var event []json.RawMessage
	if json.Unmarshal(raw, &event) != nil || len(event) == 0 {
		return VKMessage{}, false, ErrVKInvalidResponse
	}
	var eventType int
	if json.Unmarshal(event[0], &eventType) != nil {
		return VKMessage{}, false, ErrVKInvalidResponse
	}
	if eventType != 4 {
		return VKMessage{}, false, nil
	}
	if len(event) < 8 {
		return VKMessage{}, false, ErrVKInvalidResponse
	}
	var message VKMessage
	var flags int
	var extra, attachments map[string]json.RawMessage
	if json.Unmarshal(event[1], &message.ID) != nil || json.Unmarshal(event[2], &flags) != nil ||
		json.Unmarshal(event[3], &message.PeerID) != nil || json.Unmarshal(event[5], &message.Text) != nil ||
		json.Unmarshal(event[6], &extra) != nil || json.Unmarshal(event[7], &attachments) != nil {
		return VKMessage{}, false, ErrVKInvalidResponse
	}
	message.Out = (flags & 2) >> 1
	if sender, exists := extra["from"]; exists {
		var identifier json.Number
		if json.Unmarshal(sender, &identifier) != nil {
			return VKMessage{}, false, ErrVKInvalidResponse
		}
		var err error
		message.FromID, err = identifier.Int64()
		if err != nil {
			return VKMessage{}, false, ErrVKInvalidResponse
		}
	} else if message.PeerID > 0 && message.PeerID < 2000000000 && message.Out == 0 {
		message.FromID = message.PeerID
	}
	_, service := extra["source_act"]
	if service && strings.TrimSpace(message.Text) == "" && len(attachments) == 0 {
		return VKMessage{}, false, nil
	}
	message.Text = html.UnescapeString(strings.ReplaceAll(message.Text, "<br>", "\n"))
	_, forwarded := extra["fwd"]
	_, replied := extra["reply"]
	return message, len(attachments) != 0 || forwarded || replied || service, nil
}
