package main

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

type MessageStore struct{ db *sql.DB }

func OpenMessageStore(ctx context.Context, path string) (*MessageStore, error) {
	if path == "" {
		return nil, errors.New("state database path is required")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, errors.New("invalid state database path")
	}
	if err := os.MkdirAll(filepath.Dir(absolute), 0700); err != nil {
		return nil, errors.New("cannot create state directory")
	}
	if info, err := os.Lstat(absolute); err == nil {
		if !info.Mode().IsRegular() {
			return nil, errors.New("state database must be a regular file")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, errors.New("cannot inspect state database")
	}
	file, err := os.OpenFile(absolute, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, errors.New("cannot open state database file")
	}
	permissionErr := file.Chmod(0600)
	closeErr := file.Close()
	if permissionErr != nil || closeErr != nil {
		return nil, errors.New("cannot secure state database file")
	}
	endpoint := url.URL{Scheme: "file", Path: absolute}
	query := url.Values{"_pragma": {"busy_timeout(5000)"}}
	endpoint.RawQuery = query.Encode()
	db, err := sql.Open("sqlite", endpoint.String())
	if err != nil {
		return nil, errors.New("cannot open state database")
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	operation, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, err = db.ExecContext(operation, `CREATE TABLE IF NOT EXISTS message_map (
vk_message_id INTEGER PRIMARY KEY,
telegram_message_id INTEGER NOT NULL,
created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
)`)
	if err != nil {
		failure := safeTransferError(operation, "cannot initialize state database", err)
		if db.Close() != nil {
			return nil, errors.Join(failure, errors.New("cannot close state database"))
		}
		return nil, failure
	}
	return &MessageStore{db: db}, nil
}

func (store *MessageStore) Lookup(ctx context.Context, vkID int64) (int64, error) {
	operation, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var telegramID int64
	err := store.db.QueryRowContext(operation, "SELECT telegram_message_id FROM message_map WHERE vk_message_id = ?", vkID).Scan(&telegramID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, safeTransferError(operation, "state mapping lookup failed", err)
	}
	return telegramID, nil
}

func (store *MessageStore) Save(ctx context.Context, vkID, telegramID int64) error {
	if vkID <= 0 || telegramID <= 0 {
		return errors.New("invalid message mapping identifiers")
	}
	operation, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, err := store.db.ExecContext(operation, "INSERT INTO message_map (vk_message_id, telegram_message_id) VALUES (?, ?) ON CONFLICT(vk_message_id) DO NOTHING", vkID, telegramID)
	if err != nil {
		return safeTransferError(operation, "state mapping save failed", err)
	}
	return nil
}

func (store *MessageStore) Close() error {
	if err := store.db.Close(); err != nil {
		return errors.New("cannot close state database")
	}
	return nil
}
