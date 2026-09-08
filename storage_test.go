package main

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func testMessageStore(t *testing.T) *MessageStore {
	t.Helper()
	store, err := OpenMessageStore(t.Context(), filepath.Join(t.TempDir(), "mapping.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	return store
}

func TestMessageStorePersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "messages.sqlite")
	store, err := OpenMessageStore(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	if identifier, err := store.Lookup(t.Context(), 42); err != nil || identifier != 0 {
		t.Fatalf("new schema lookup: %d %v", identifier, err)
	}
	if err := store.Save(t.Context(), 42, 1001); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(t.Context(), 42, 1001); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(t.Context(), 42, 9999); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = OpenMessageStore(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	identifier, err := store.Lookup(t.Context(), 42)
	if err != nil || identifier != 1001 {
		t.Fatalf("first mapping not preserved on restart: %d %v", identifier, err)
	}
	var created string
	if err := store.db.QueryRowContext(t.Context(), "SELECT created_at FROM message_map WHERE vk_message_id = ?", 42).Scan(&created); err != nil || created == "" {
		t.Fatal("missing creation timestamp")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := store.Lookup(ctx, 42); !errors.Is(err, context.Canceled) {
		t.Fatalf("lookup cancellation: %v", err)
	}
	if err := store.Save(ctx, 43, 1002); !errors.Is(err, context.Canceled) {
		t.Fatalf("save cancellation: %v", err)
	}
}

func TestVKRateStatePersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.sqlite")
	store, err := OpenMessageStore(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	wantUntil := time.Unix(2_000_000_000, 0).UTC()
	if err := store.SaveVKRateState(t.Context(), wantUntil, 2); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = OpenMessageStore(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	until, level, err := store.LoadVKRateState(t.Context())
	if err != nil || !until.Equal(wantUntil) || level != 2 {
		t.Fatalf("rate state not restored: until=%v level=%d err=%v", until, level, err)
	}
}
