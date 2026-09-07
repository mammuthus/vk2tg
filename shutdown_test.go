package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestLongPollShutdownAndSafeLogs(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var logs bytes.Buffer
	polls := 0
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/messages.getLongPollServer":
			writeFixture(t, writer, fmt.Sprintf(`{"response":{"server":%q,"key":"private-poll-key","ts":"10"}}`, server.URL+"/poll"))
		case "/poll":
			polls++
			if polls == 1 {
				writeFixture(t, writer, `{"ts":"11","updates":[]}`)
				return
			}
			cancel()
			<-request.Context().Done()
		default:
			t.Error("unexpected endpoint")
			writer.WriteHeader(400)
		}
	}))
	defer server.Close()
	client, err := NewVKClient(Config{VKAccessToken: "private-token"}, server.URL, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	relay := Relay{vk: client, logger: slog.New(slog.NewJSONHandler(&logs, nil))}
	if err := relay.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("shutdown did not cancel polling: %v", err)
	}
	for _, event := range []string{"vk long poll connected", "vk long poll cycle complete"} {
		if !strings.Contains(logs.String(), event) {
			t.Fatal("missing operational log")
		}
	}
	for _, private := range []string{"private-poll-key", "private-token", server.URL} {
		if strings.Contains(logs.String(), private) {
			t.Fatal("operational log leaked secret or URL")
		}
	}
}
