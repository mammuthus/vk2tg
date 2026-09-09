package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

func newTestTelegramClient(config Config, baseURL string, timeout time.Duration) (*TelegramClient, error) {
	client, err := NewTelegramClient(config, baseURL, timeout)
	if err != nil {
		return nil, err
	}
	clock := &fakeVKClock{now: time.Unix(1_900_000_000, 0)}
	client.pacer.now, client.pacer.wait = clock.Now, clock.Wait
	return client, nil
}

type telegramRateRoundTripper func(*http.Request) (*http.Response, error)

func (transport telegramRateRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport(request)
}

func TestTelegramPacingBurstAndIdle(t *testing.T) {
	clock := &fakeVKClock{now: time.Unix(1_900_000_000, 0)}
	var times []time.Time
	var methods []string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		times = append(times, clock.Now())
		methods = append(methods, request.URL.Path)
		writeTelegramSuccess(t, writer, request)
	}))
	defer server.Close()
	client, err := NewTelegramClient(Config{TelegramBotToken: "fake-token", TelegramTargetChatID: -123}, server.URL, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	client.pacer.now, client.pacer.wait = clock.Now, clock.Wait
	start := clock.Now()
	for _, method := range []string{"sendMessage", "sendPhoto", "sendDocument"} {
		if _, err := client.send(t.Context(), method, "application/json", bytes.NewReader([]byte(`{}`))); err != nil {
			t.Fatal(err)
		}
	}
	if err := clock.Wait(t.Context(), 10*time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err := client.SendMessage(t.Context(), "idle", 0); err != nil {
		t.Fatal(err)
	}
	for index, offset := range []time.Duration{0, time.Second, 2 * time.Second, 12 * time.Second} {
		if !times[index].Equal(start.Add(offset)) {
			t.Fatalf("send %d at %v", index, times[index].Sub(start))
		}
	}
	if len(methods) != 4 {
		t.Fatal("unexpected request count")
	}
}

func TestTelegramPacingRetryAfter(t *testing.T) {
	clock := &fakeVKClock{now: time.Unix(1_900_000_000, 0)}
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests++
		if requests == 1 {
			writer.WriteHeader(429)
			writeFixture(t, writer, `{"ok":false,"error_code":429,"parameters":{"retry_after":7}}`)
			return
		}
		writeTelegramSuccess(t, writer, request)
	}))
	defer server.Close()
	client, err := NewTelegramClient(Config{TelegramBotToken: "fake-token", TelegramTargetChatID: -123}, server.URL, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	client.pacer.now, client.pacer.wait = clock.Now, clock.Wait
	if _, err := client.SendMessage(t.Context(), "test", 0); err != nil {
		t.Fatal(err)
	}
	if requests != 2 || !reflect.DeepEqual(clock.waits, []time.Duration{7 * time.Second}) {
		t.Fatalf("requests=%d waits=%v", requests, clock.waits)
	}
}

func TestTelegramPacingAlbumCostAndCancellation(t *testing.T) {
	clock := &fakeVKClock{now: time.Unix(1_900_000_000, 0)}
	pacer := newTelegramPacer()
	pacer.now, pacer.wait = clock.Now, clock.Wait
	if err := pacer.reserve(t.Context(), 10); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	pacer.wait = func(ctx context.Context, delay time.Duration) error {
		if delay != 10*time.Second {
			t.Fatalf("album cost lost: %v", delay)
		}
		cancel()
		return ctx.Err()
	}
	if err := pacer.reserve(ctx, 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("wait cancellation: %v", err)
	}
	pacer.wait = clock.Wait
	if err := pacer.reserve(t.Context(), 1); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(clock.waits, []time.Duration{10 * time.Second}) {
		t.Fatal("cancelled send consumed capacity")
	}
	if err := pacer.acquire(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := pacer.acquire(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("queue cancellation: %v", err)
	}
	pacer.release()
}

func TestTelegramPacingGroupFromSendResponse(t *testing.T) {
	for _, chatType := range []string{"channel", "private", "group", "supergroup"} {
		t.Run(chatType, func(t *testing.T) {
			clock := &fakeVKClock{now: time.Unix(1_900_000_000, 0)}
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.URL.Path != "/botfake-token/sendMessage" {
					t.Error("unexpected metadata probe")
				}
				io.WriteString(writer, `{"ok":true,"result":{"message_id":101,"chat":{"type":"`+chatType+`"}}}`)
			}))
			defer server.Close()
			client, err := NewTelegramClient(Config{TelegramBotToken: "fake-token", TelegramTargetChatID: -123}, server.URL, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			client.pacer.now, client.pacer.wait = clock.Now, clock.Wait
			for range 3 {
				if _, err := client.SendMessage(t.Context(), "test", 0); err != nil {
					t.Fatal(err)
				}
			}
			interval := time.Second
			if chatType == "group" || chatType == "supergroup" {
				interval = 3 * time.Second
			}
			if !reflect.DeepEqual(clock.waits, []time.Duration{interval, interval}) {
				t.Fatalf("wrong pacing: %v", clock.waits)
			}
		})
	}
}

func TestTelegramPacingConcurrentRetryAndCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client, err := NewTelegramClient(Config{TelegramBotToken: "fake-token", TelegramTargetChatID: -123}, "https://telegram.test", time.Second)
		if err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		var times []time.Duration
		var bodies []string
		client.httpClient.Transport = telegramRateRoundTripper(func(request *http.Request) (*http.Response, error) {
			times = append(times, time.Since(start))
			body, err := io.ReadAll(request.Body)
			if err != nil {
				t.Error(err)
			}
			bodies = append(bodies, string(body))
			response := `{"ok":true,"result":{"message_id":101}}`
			status := 200
			if len(times) == 1 {
				response = `{"ok":false,"error_code":429,"parameters":{"retry_after":7}}`
				status = 429
			}
			return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(response)), Header: make(http.Header)}, nil
		})
		firstDone := make(chan error, 1)
		go func() { _, err := client.SendMessage(t.Context(), "first", 0); firstDone <- err }()
		synctest.Wait()
		ctx, cancel := context.WithCancel(t.Context())
		cancelledDone := make(chan error, 1)
		go func() { _, err := client.SendMessage(ctx, "cancelled", 0); cancelledDone <- err }()
		synctest.Wait()
		cancel()
		if err := <-cancelledDone; !errors.Is(err, context.Canceled) {
			t.Fatalf("queued cancellation: %v", err)
		}
		nextDone := make(chan error, 1)
		go func() { _, err := client.SendMessage(t.Context(), "next", 0); nextDone <- err }()
		synctest.Wait()
		if err := <-firstDone; err != nil {
			t.Fatal(err)
		}
		if err := <-nextDone; err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(times, []time.Duration{0, 7 * time.Second, 8 * time.Second}) {
			t.Fatalf("unexpected sends: %v", times)
		}
		if bodies[0] != bodies[1] {
			t.Fatal("retry body changed")
		}
	})
}

func TestTelegramPacing50MessagesAcrossPipeline(t *testing.T) {
	clock := &fakeVKClock{now: time.Unix(1_900_000_000, 0)}
	var times []time.Time
	var methods []string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		times = append(times, clock.Now())
		methods = append(methods, request.URL.Path)
		if !strings.HasSuffix(request.URL.Path, "sendMessage") {
			if err := request.ParseMultipartForm(1 << 20); err != nil {
				t.Error(err)
				return
			}
			defer request.MultipartForm.RemoveAll()
		}
		writeTelegramSuccess(t, writer, request)
	}))
	defer server.Close()
	client, err := NewTelegramClient(Config{TelegramBotToken: "fake-token", TelegramTargetChatID: -123}, server.URL, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	client.pacer.now, client.pacer.wait = clock.Now, clock.Wait
	directory := t.TempDir()
	file := filepath.Join(directory, "media")
	if err := os.WriteFile(file, []byte("mock media"), 0600); err != nil {
		t.Fatal(err)
	}
	photo := localMedia{MediaSource: MediaSource{Kind: "photo", Name: "photo.jpg"}, Path: file}
	document := localMedia{MediaSource: MediaSource{Kind: "document", Name: "file.txt"}, Path: file}
	start := clock.Now()
	for index := range 50 {
		var err error
		switch index {
		case 1:
			_, err = client.sendFiles(t.Context(), directory, []localMedia{photo}, "photo", 0)
		case 2:
			_, err = client.sendFiles(t.Context(), directory, []localMedia{document}, "document", 0)
		case 3:
			_, err = client.sendFiles(t.Context(), directory, []localMedia{photo, photo}, "album", 0)
		default:
			_, err = client.SendMessage(t.Context(), "burst", 0)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(times) != 50 {
		t.Fatalf("unexpected HTTP requests: %d", len(times))
	}
	for index, sent := range times {
		offset := time.Duration(index) * time.Second
		if index > 3 {
			offset += time.Second
		}
		if !sent.Equal(start.Add(offset)) {
			t.Fatalf("request %d timing=%v want=%v", index, sent.Sub(start), offset)
		}
	}
	if !reflect.DeepEqual(methods[:4], []string{"/botfake-token/sendMessage", "/botfake-token/sendPhoto", "/botfake-token/sendDocument", "/botfake-token/sendMediaGroup"}) {
		t.Fatalf("incorrect pipeline methods: %v", methods[:4])
	}
}
