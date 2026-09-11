package main

import (
	"context"
	"errors"
	"time"
)

type migrationState struct {
	PeerID, ChatID, LastID, PendingID     int64
	Total, Offset, Sent, Skipped, Batches int
	PauseSeconds                          int64
	NotBefore                             int64
	Completed                             bool
}

func (store *MessageStore) loadMigration(ctx context.Context, peerID, chatID int64) (migrationState, error) {
	operation, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, err := store.db.ExecContext(operation, `CREATE TABLE IF NOT EXISTS history_migration (
singleton INTEGER PRIMARY KEY CHECK(singleton=1),
peer_id INTEGER NOT NULL, chat_id INTEGER NOT NULL,
total INTEGER NOT NULL DEFAULT 0, offset INTEGER NOT NULL DEFAULT 0,
last_id INTEGER NOT NULL DEFAULT 0, pending_id INTEGER NOT NULL DEFAULT 0,
sent INTEGER NOT NULL DEFAULT 0, skipped INTEGER NOT NULL DEFAULT 0,
batches INTEGER NOT NULL DEFAULT 0, pause_seconds INTEGER NOT NULL DEFAULT 60,
not_before INTEGER NOT NULL DEFAULT 0, completed INTEGER NOT NULL DEFAULT 0
)`)
	if err != nil {
		return migrationState{}, safeTransferError(operation, "cannot initialize migration progress", err)
	}
	_, err = store.db.ExecContext(operation, `INSERT INTO history_migration(singleton,peer_id,chat_id) VALUES(1,?,?) ON CONFLICT(singleton) DO NOTHING`, peerID, chatID)
	if err != nil {
		return migrationState{}, safeTransferError(operation, "cannot initialize migration identity", err)
	}
	var state migrationState
	err = store.db.QueryRowContext(operation, `SELECT peer_id,chat_id,total,offset,last_id,pending_id,sent,skipped,batches,pause_seconds,not_before,completed FROM history_migration WHERE singleton=1`).Scan(
		&state.PeerID, &state.ChatID, &state.Total, &state.Offset, &state.LastID, &state.PendingID, &state.Sent, &state.Skipped, &state.Batches, &state.PauseSeconds, &state.NotBefore, &state.Completed)
	if err != nil {
		return migrationState{}, safeTransferError(operation, "cannot load migration progress", err)
	}
	if state.PeerID != peerID || state.ChatID != chatID {
		return migrationState{}, errors.New("migration source or destination changed")
	}
	if state.Offset < 0 || state.Total < state.Offset || state.Sent < 0 || state.Skipped < 0 || state.Sent+state.Skipped != state.Offset || state.PauseSeconds < 60 || state.LastID < 0 || state.PendingID < 0 {
		return migrationState{}, errors.New("invalid migration progress")
	}
	return state, nil
}

func (store *MessageStore) saveMigration(ctx context.Context, state migrationState) error {
	operation, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, err := store.db.ExecContext(operation, `UPDATE history_migration SET total=?,offset=?,last_id=?,pending_id=?,sent=?,skipped=?,batches=?,pause_seconds=?,not_before=?,completed=? WHERE singleton=1 AND peer_id=? AND chat_id=?`,
		state.Total, state.Offset, state.LastID, state.PendingID, state.Sent, state.Skipped, state.Batches, state.PauseSeconds, state.NotBefore, state.Completed, state.PeerID, state.ChatID)
	if err != nil {
		return safeTransferError(operation, "cannot save migration progress", err)
	}
	return nil
}
