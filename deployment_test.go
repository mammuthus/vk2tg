package main

import (
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDeploymentVolumePersistence(t *testing.T) {
	mode := os.Getenv("VK2TG_VOLUME_TEST")
	path := filepath.Join(t.TempDir(), "deployment-check.sqlite")
	if mode != "" {
		if mode != "seed" && mode != "verify" {
			t.Fatal("invalid volume test mode")
		}
		path = "/data/deployment-check.sqlite"
	}
	if mode != "verify" {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatal("fixture path already exists")
		}
		store, err := OpenMessageStore(t.Context(), path)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Save(t.Context(), 42, 1001); err != nil {
			t.Fatal(err)
		}
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
		if mode == "seed" {
			return
		}
	}
	store, err := OpenMessageStore(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	identifier, err := store.Lookup(t.Context(), 42)
	if err != nil || identifier != 1001 {
		t.Fatal("persisted fixture mapping lost")
	}
	sends := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		sends++
		var payload struct {
			Reply *TelegramReplyParameters `json:"reply_parameters"`
		}
		if request.URL.Path != "/botfake-token/sendMessage" || json.NewDecoder(request.Body).Decode(&payload) != nil || payload.Reply == nil || payload.Reply.MessageID != 1001 {
			t.Error("persisted target missing from reply")
		}
		writeFixture(t, writer, `{"ok":true,"result":{"message_id":1002}}`)
	}))
	defer server.Close()
	telegram, err := NewTelegramClient(Config{TelegramBotToken: "fake-token", TelegramTargetChatID: -123}, server.URL, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	relay := Relay{store: store, telegram: telegram}
	message := RenderedMessage{VKMessageID: 43, VKReplyToID: 42, Name: "Sender", Text: "fixture reply"}
	if err := relay.deliverMapped(t.Context(), message); err != nil {
		t.Fatal(err)
	}
	if err := relay.deliverMapped(t.Context(), message); err != nil {
		t.Fatal(err)
	}
	if sends != 1 {
		t.Fatal("persisted mapping did not prevent duplicate")
	}
	identifier, err = store.Lookup(t.Context(), 43)
	if err != nil || identifier != 1002 {
		t.Fatal("reply mapping not stored")
	}
}

func TestDeploymentStateAudit(t *testing.T) {
	if os.Getenv("VK2TG_STATE_AUDIT") != "1" {
		t.Skip("container-only read-only live state audit")
	}
	info, err := os.Stat("/data/state.db")
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("live state missing or incorrect permissions")
	}
	db, err := sql.Open("sqlite", "file:/data/state.db?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal("cannot open read-only state")
	}
	defer db.Close()
	var integrity string
	if err := db.QueryRowContext(t.Context(), "PRAGMA quick_check").Scan(&integrity); err != nil || integrity != "ok" {
		t.Fatal("state integrity check failed")
	}
	rows, err := db.QueryContext(t.Context(), "SELECT vk_message_id, telegram_message_id, created_at FROM message_map ORDER BY vk_message_id")
	if err != nil {
		t.Fatal("mapping table unavailable")
	}
	defer rows.Close()
	fingerprint := sha256.New()
	count := 0
	for rows.Next() {
		var vkID, telegramID int64
		var created string
		if err := rows.Scan(&vkID, &telegramID, &created); err != nil {
			t.Fatal("invalid state row")
		}
		fmt.Fprintf(fingerprint, "%d:%d:%s\n", vkID, telegramID, created)
		count++
	}
	if rows.Err() != nil {
		t.Fatal("state read failed")
	}
	t.Logf("state_integrity=ok mapping_rows=%d fingerprint=%x", count, fingerprint.Sum(nil))
}
