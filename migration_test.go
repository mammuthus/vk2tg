package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestHistoryPagePagination(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.ParseForm() != nil || request.URL.Path != "/messages.getHistory" || request.Form.Get("offset") != "50" || request.Form.Get("rev") != "1" || request.Form.Get("count") != "50" {
			t.Error("wrong pagination request")
		}
		writeFixture(t, writer, `{"response":{"count":52,"items":[{"id":101,"peer_id":2000000001,"from_id":42,"out":1},{"id":105,"peer_id":2000000001,"from_id":42}]}}`)
	}))
	defer server.Close()
	client, err := NewVKClient(Config{VKAccessToken: "fake-token"}, server.URL, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	page, err := client.GetHistoryPage(t.Context(), 2000000001, 50, 50)
	if err != nil || page.Count != 52 || len(page.Items) != 2 || page.Items[0].ID != 101 || page.Items[1].ID != 105 || page.Items[0].Out != 1 {
		t.Fatalf("incorrect oldest-first page: %v", err)
	}
}

type migrationTestClock struct {
	now       time.Time
	waits     []time.Duration
	interrupt bool
}

func (clock *migrationTestClock) Now() time.Time { return clock.now }
func (clock *migrationTestClock) Wait(ctx context.Context, delay time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	clock.waits = append(clock.waits, delay)
	if clock.interrupt {
		return context.Canceled
	}
	clock.now = clock.now.Add(delay)
	return nil
}

func TestMigrationOrderMappingRepliesAndResume(t *testing.T) {
	path := filepath.Join(t.TempDir(), "migration.sqlite")
	store, err := OpenMessageStore(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&output, &slog.HandlerOptions{Level: slog.LevelDebug}))
	var sentIDs []int64
	var offsets []int
	usersCalls, telegramCalls := 0, 0
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/messages.getHistory":
			if request.ParseForm() != nil || request.Form.Get("rev") != "1" {
				t.Error("not oldest-first")
			}
			offset, _ := strconv.Atoi(request.Form.Get("offset"))
			count, _ := strconv.Atoi(request.Form.Get("count"))
			offsets = append(offsets, offset)
			items := make([]map[string]any, 0)
			for position := offset; position < 53 && position < offset+count; position++ {
				identifier := 100 + position
				message := map[string]any{"id": identifier, "peer_id": 2000000001, "from_id": 42, "out": 1, "text": fmt.Sprintf("message %d", identifier)}
				if position > 0 {
					message["reply_message"] = map[string]int{"id": identifier - 1}
				}
				if identifier == 102 {
					message["from_id"] = 73
				}
				if identifier == 103 {
					message["text"] = ""
					message["action"] = map[string]string{"type": "chat_invite_user"}
				}
				if identifier == 101 {
					message["attachments"] = []any{map[string]any{"type": "photo", "photo": map[string]any{"sizes": []any{map[string]any{"url": server.URL + "/photo", "width": 20, "height": 20}}}}}
				}
				items = append(items, message)
			}
			encoded, _ := json.Marshal(map[string]any{"response": map[string]any{"count": 53, "items": items}})
			writeFixture(t, writer, string(encoded))
		case "/users.get":
			usersCalls++
			writeFixture(t, writer, `{"response":[{"id":42,"first_name":"Sender"}]}`)
		case "/photo":
			writeFixture(t, writer, "photo bytes")
		case "/botfake-token/sendPhoto", "/botfake-token/sendMessage":
			telegramCalls++
			if telegramCalls == 1 {
				writer.WriteHeader(429)
				writeFixture(t, writer, `{"ok":false,"error_code":429,"parameters":{"retry_after":3}}`)
				return
			}
			var payload struct {
				Text  string                   `json:"text"`
				Reply *TelegramReplyParameters `json:"reply_parameters"`
			}
			if strings.HasSuffix(request.URL.Path, "sendPhoto") {
				if request.ParseMultipartForm(1<<20) != nil {
					t.Error("bad photo")
					return
				}
				defer request.MultipartForm.RemoveAll()
				payload.Text = request.FormValue("caption")
				if value := request.FormValue("reply_parameters"); value != "" {
					if json.Unmarshal([]byte(value), &payload.Reply) != nil {
						t.Error("bad reply")
					}
				}
			} else if json.NewDecoder(request.Body).Decode(&payload) != nil {
				t.Error("bad message")
				return
			}
			_, number, ok := strings.Cut(payload.Text, "message ")
			identifier, err := strconv.ParseInt(number, 10, 64)
			if !ok || err != nil {
				t.Error("missing source identifier")
				return
			}
			if len(sentIDs) > 0 && identifier <= sentIDs[len(sentIDs)-1] {
				t.Error("duplicate or out-of-order send")
			}
			prior, err := store.Lookup(t.Context(), identifier-1)
			if err != nil {
				t.Error(err)
			}
			if prior != 0 && (payload.Reply == nil || payload.Reply.MessageID != prior) {
				t.Error("new mapping not used for reply")
			}
			if prior == 0 && payload.Reply != nil {
				t.Error("unknown reply target was not omitted")
			}
			sentIDs = append(sentIDs, identifier)
			writeFixture(t, writer, fmt.Sprintf(`{"ok":true,"result":{"message_id":%d,"chat":{"type":"channel"}}}`, 1000+identifier))
		default:
			t.Error("unnecessary API request")
			writer.WriteHeader(400)
		}
	}))
	defer server.Close()
	config := Config{Debug: true, VKAccessToken: "fake-token", VKTargetPeerID: 2000000001, TelegramBotToken: "fake-token", TelegramTargetChatID: -123, VKBlockedSenderIDs: map[int64]struct{}{73: {}}}
	clock := &migrationTestClock{now: time.Unix(2000000000, 0), interrupt: true}
	newRelay := func() *Relay {
		vk, err := NewVKClient(config, server.URL, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		telegram, err := newTestTelegramClient(config, server.URL, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		return &Relay{config: config, vk: vk, telegram: telegram, store: store, logger: logger, mediaHTTP: server.Client(), tempRoot: t.TempDir(), migrationClock: clock}
	}
	firstRelay := newRelay()
	if err := firstRelay.MigrateHistory(t.Context()); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected interrupted batch pause: %v", err)
	}
	state, err := store.loadMigration(t.Context(), config.VKTargetPeerID, config.TelegramTargetChatID)
	if err != nil || state.Offset != 50 || state.Sent != 48 || state.Skipped != 2 || state.PendingID != 0 || state.PauseSeconds != 120 {
		t.Fatalf("bad checkpoint: %+v %v", state, err)
	}
	if len(clock.waits) != 1 || clock.waits[0] < 120*time.Second {
		t.Fatal("429 did not increase batch pause")
	}
	if firstRelay.telegram.stats.snapshot().RateLimits != 1 || firstRelay.telegram.stats.snapshot().Retries != 1 {
		t.Fatal("bad retry accounting")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = OpenMessageStore(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	clock.interrupt = false
	secondRelay := newRelay()
	if err := secondRelay.MigrateHistory(t.Context()); err != nil {
		t.Fatal(err)
	}
	state, err = store.loadMigration(t.Context(), config.VKTargetPeerID, config.TelegramTargetChatID)
	if err != nil || !state.Completed || state.Sent != 51 || state.Skipped != 2 || state.Offset != 53 || state.Batches != 2 {
		t.Fatalf("bad final progress: %+v %v", state, err)
	}
	if len(offsets) != 2 || offsets[0] != 0 || offsets[1] != 49 || usersCalls != 2 || len(sentIDs) != 51 {
		t.Fatalf("incorrect resume work: offsets=%v users=%d sends=%d", offsets, usersCalls, len(sentIDs))
	}
	for _, identifier := range sentIDs {
		mapped, err := store.Lookup(t.Context(), identifier)
		if err != nil || mapped != 1000+identifier {
			t.Fatal("successful message mapping not persisted")
		}
	}
	if err := secondRelay.MigrateHistory(t.Context()); err != nil || len(offsets) != 2 || len(sentIDs) != 51 {
		t.Fatal("completed migration repeated work")
	}
}

func TestMigrationPendingDeliveryGuard(t *testing.T) {
	for _, mapped := range []bool{false, true} {
		t.Run(fmt.Sprint(mapped), func(t *testing.T) {
			store := testMessageStore(t)
			state, err := store.loadMigration(t.Context(), 2000000001, -123)
			if err != nil {
				t.Fatal(err)
			}
			state.Total, state.PendingID = 1, 100
			if err := store.saveMigration(t.Context(), state); err != nil {
				t.Fatal(err)
			}
			if mapped {
				if err := store.Save(t.Context(), 100, 1100); err != nil {
					t.Fatal(err)
				}
			}
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				requests++
				if request.URL.Path != "/messages.getHistory" {
					t.Error("unexpected resend")
					writer.WriteHeader(400)
					return
				}
				writeFixture(t, writer, `{"response":{"count":1,"items":[{"id":100,"peer_id":2000000001,"from_id":42,"text":"already sent"}]}}`)
			}))
			defer server.Close()
			config := Config{VKAccessToken: "fake-token", VKTargetPeerID: 2000000001, TelegramBotToken: "fake-token", TelegramTargetChatID: -123}
			vk, err := NewVKClient(config, server.URL, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			telegram, err := newTestTelegramClient(config, server.URL, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			relay := Relay{config: config, vk: vk, telegram: telegram, store: store, logger: slog.New(slog.DiscardHandler), migrationClock: &migrationTestClock{now: time.Unix(2000000000, 0)}}
			err = relay.MigrateHistory(t.Context())
			if mapped {
				if err != nil || requests != 1 {
					t.Fatalf("mapped pending recovery failed: %v", err)
				}
			} else if err == nil || requests != 0 {
				t.Fatal("ambiguous delivery retried")
			}
		})
	}
}

func TestMigrationFloodStops(t *testing.T) {
	for _, code := range []int{9, 29} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				calls++
				writeFixture(t, writer, fmt.Sprintf(`{"error":{"error_code":%d}}`, code))
			}))
			defer server.Close()
			config := Config{VKAccessToken: "fake-token", VKTargetPeerID: 2000000001, TelegramBotToken: "fake-token", TelegramTargetChatID: -123}
			vk, err := NewVKClient(config, server.URL, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			telegram, err := newTestTelegramClient(config, server.URL, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			store := testMessageStore(t)
			if err := vk.ConfigureRateProtection(t.Context(), store, slog.New(slog.DiscardHandler)); err != nil {
				t.Fatal(err)
			}
			relay := Relay{config: config, vk: vk, telegram: telegram, store: store, logger: slog.New(slog.DiscardHandler)}
			var failure *VKAPIError
			if err := relay.MigrateHistory(t.Context()); !errors.As(err, &failure) || failure.Code != code || calls != 1 {
				t.Fatalf("flood did not stop: %v calls=%d", err, calls)
			}
			until, level, err := store.LoadVKRateState(t.Context())
			if err != nil || level != 1 || !until.After(time.Now()) {
				t.Fatal("flood cooldown not persisted")
			}
		})
	}
}

func TestMigrationBoundaryChangeStops(t *testing.T) {
	store := testMessageStore(t)
	state, err := store.loadMigration(t.Context(), 2000000001, -123)
	if err != nil {
		t.Fatal(err)
	}
	state.Total, state.Offset, state.Sent, state.LastID = 2, 1, 1, 100
	if err := store.saveMigration(t.Context(), state); err != nil {
		t.Fatal(err)
	}
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests++
		writeFixture(t, writer, `{"response":{"count":2,"items":[{"id":101,"peer_id":2000000001,"from_id":42}]}}`)
	}))
	defer server.Close()
	config := Config{VKAccessToken: "fake-token", VKTargetPeerID: 2000000001, TelegramBotToken: "fake-token", TelegramTargetChatID: -123}
	vk, err := NewVKClient(config, server.URL, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	telegram, err := newTestTelegramClient(config, server.URL, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	relay := Relay{config: config, vk: vk, telegram: telegram, store: store, logger: slog.New(slog.DiscardHandler)}
	if err := relay.MigrateHistory(t.Context()); err == nil || !strings.Contains(err.Error(), "boundary changed") || requests != 1 {
		t.Fatalf("shifted pagination was not stopped: %v", err)
	}
}

func TestMigrationSendFailureLeavesPending(t *testing.T) {
	store := testMessageStore(t)
	sends := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/messages.getHistory":
			writeFixture(t, writer, `{"response":{"count":1,"items":[{"id":100,"peer_id":2000000001,"from_id":42,"out":1,"text":"message"}]}}`)
		case "/botfake-token/sendMessage":
			sends++
			writer.WriteHeader(502)
		default:
			t.Error("unexpected metadata request")
			writer.WriteHeader(400)
		}
	}))
	defer server.Close()
	config := Config{VKAccessToken: "fake-token", VKTargetPeerID: 2000000001, TelegramBotToken: "fake-token", TelegramTargetChatID: -123}
	vk, err := NewVKClient(config, server.URL, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	telegram, err := newTestTelegramClient(config, server.URL, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	relay := Relay{config: config, vk: vk, telegram: telegram, store: store, logger: slog.New(slog.DiscardHandler), senderNames: map[int64]string{42: "Sender"}}
	if err := relay.MigrateHistory(t.Context()); err == nil || sends != 1 {
		t.Fatal("failed send not reported")
	}
	state, err := store.loadMigration(t.Context(), config.VKTargetPeerID, config.TelegramTargetChatID)
	if err != nil || state.PendingID != 100 || state.Offset != 0 || state.Sent != 0 {
		t.Fatal("ambiguous delivery marker lost")
	}
	mapped, err := store.Lookup(t.Context(), 100)
	if err != nil || mapped != 0 {
		t.Fatal("failed send created mapping")
	}
	if err := relay.MigrateHistory(t.Context()); err == nil || sends != 1 {
		t.Fatal("uncertain send repeated on resume")
	}
}

func TestMigrationCommand(t *testing.T) {
	mode, err := parseHistoryCommand([]string{"migrate-history"})
	if err != nil || mode != -1 {
		t.Fatal("migration command not recognized")
	}
	if _, err := parseHistoryCommand([]string{"migrate-history", "extra"}); err == nil {
		t.Fatal("unexpected arguments accepted")
	}
}
