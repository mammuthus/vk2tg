package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestLongPollEvent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		query := request.URL.Query()
		if query.Get("act") != "a_check" || query.Get("key") != "fake-key" || query.Get("ts") != "10" || query.Get("version") != "3" || query.Get("mode") != "2" {
			t.Error("incorrect Long Poll parameters")
		}
		writeFixture(t, writer, `{"ts":"11","updates":[[4,99,1,2000000001,123,"hello &amp; bye",{"from":"42"},{}]]}`)
	}))
	defer server.Close()
	client, err := NewVKClient(Config{VKAccessToken: "fake-token"}, server.URL, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	batch, err := client.WaitLongPoll(t.Context(), VKLongPollServer{Server: server.URL, Key: "fake-key", TS: json.Number("10")})
	if err != nil {
		t.Fatal(err)
	}
	message, full, err := decodeLongPollMessage(batch.Updates[0])
	if err != nil || full || message.ID != 99 || message.PeerID != 2000000001 || message.FromID != 42 || message.Out != 0 || message.Text != "hello & bye" || batch.TS != "11" {
		t.Fatalf("unexpected decoded event: full=%v err=%v", full, err)
	}
}

func TestLongPollTimeoutSafeError(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		select {
		case <-request.Context().Done():
		case <-release:
		}
	}))
	defer server.Close()
	defer close(release)
	client, err := NewVKClient(Config{VKAccessToken: "fake-token"}, server.URL, 30*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.WaitLongPoll(t.Context(), VKLongPollServer{Server: server.URL, Key: "private-longpoll-key", TS: "10"})
	if !errors.Is(err, context.DeadlineExceeded) || strings.Contains(err.Error(), "private-longpoll-key") || strings.Contains(err.Error(), server.URL) {
		t.Fatalf("unsafe timeout error: %v", err)
	}
}

func TestLongPollDirectAndOtherEvents(t *testing.T) {
	for _, testCase := range []struct {
		name, raw string
		sender    int64
		out       int
		full      bool
	}{
		{name: "direct incoming", raw: `[4,1,1,42,1,"text",{},{}]`, sender: 42},
		{name: "group sender", raw: `[4,1,0,2000000001,1,"text",{"from":"-42"},{}]`, sender: -42},
		{name: "outbox", raw: `[4,1,3,2000000001,1,"text",{"from":"42"},{}]`, sender: 42, out: 1},
		{name: "reply", raw: `[4,1,1,42,1,"text",{"reply":"123"},{}]`, sender: 42, full: true},
		{name: "not new message", raw: `[5,1,0,42,1,"edited",{},{}]`},
		{name: "service event", raw: `[4,1,0,2000000001,1,"",{"from":"42","source_act":"chat_invite_user"},{}]`},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			message, full, err := decodeLongPollMessage(json.RawMessage(testCase.raw))
			if err != nil || message.FromID != testCase.sender || message.Out != testCase.out || full != testCase.full {
				t.Fatalf("incorrect event: %v", err)
			}
		})
	}
}

func writeFixture(t *testing.T, writer http.ResponseWriter, body string) {
	t.Helper()
	if _, err := writer.Write([]byte(body)); err != nil {
		t.Error(err)
	}
}
