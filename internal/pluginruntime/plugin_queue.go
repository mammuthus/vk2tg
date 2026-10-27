package pluginruntime

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// Queue data is separate from the relay's mappings and rate-limit state.
func openQueue(ctx context.Context, path string) (*sql.DB, error) {
	if path == "" {
		return nil, errors.New("plugin queue path is required")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, errors.New("invalid plugin queue path")
	}
	if err = os.MkdirAll(filepath.Dir(absolute), 0700); err != nil {
		return nil, errors.New("cannot create plugin queue directory")
	}
	if info, statErr := os.Lstat(absolute); statErr == nil {
		if !info.Mode().IsRegular() {
			return nil, errors.New("plugin queue must be a regular file")
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return nil, errors.New("cannot inspect plugin queue")
	}
	file, err := os.OpenFile(absolute, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, errors.New("cannot open plugin queue")
	}
	permissionErr, closeErr := file.Chmod(0600), file.Close()
	if permissionErr != nil || closeErr != nil {
		return nil, errors.New("cannot secure plugin queue")
	}
	endpoint := url.URL{Scheme: "file", Path: absolute}
	endpoint.RawQuery = url.Values{"_pragma": {"busy_timeout(100)"}}.Encode()
	db, err := sql.Open("sqlite", endpoint.String())
	if err != nil {
		return nil, errors.New("cannot open plugin queue")
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	operation, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, err = db.ExecContext(operation, `CREATE TABLE IF NOT EXISTS plugin_jobs (
 plugin TEXT NOT NULL,
 peer_id INTEGER NOT NULL,
 message_id INTEGER NOT NULL,
 destination_id INTEGER NOT NULL,
 payload BLOB,
 attempts INTEGER NOT NULL DEFAULT 0,
 available_at INTEGER NOT NULL DEFAULT 0,
 PRIMARY KEY(plugin, peer_id, message_id, destination_id)
 );
 CREATE INDEX IF NOT EXISTS plugin_jobs_ready ON plugin_jobs(plugin, available_at) WHERE payload IS NOT NULL;`)
	if err != nil {
		db.Close()
		return nil, errors.New("cannot initialize plugin queue")
	}
	return db, nil
}

type job struct {
	id       int64
	payload  []byte
	attempts int
}

func (m *Manager) next(ctx context.Context, name string) (job, error) {
	operation, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	var item job
	err := m.db.QueryRowContext(operation, `SELECT rowid, payload, attempts FROM plugin_jobs
 WHERE plugin = ? AND payload IS NOT NULL AND available_at <= ? ORDER BY rowid LIMIT 1`, name, time.Now().UnixMilli()).Scan(&item.id, &item.payload, &item.attempts)
	return item, err
}

func (m *Manager) finish(ctx context.Context, item job, succeeded bool) error {
	operation, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if succeeded {
		// Keep the deduplication key, but erase message content after acknowledgement.
		_, err := m.db.ExecContext(operation, "UPDATE plugin_jobs SET payload = NULL WHERE rowid = ?", item.id)
		return err
	}
	delay := time.Second << min(item.attempts, 8)
	_, err := m.db.ExecContext(operation, "UPDATE plugin_jobs SET attempts = attempts + 1, available_at = ? WHERE rowid = ?", time.Now().Add(delay).UnixMilli(), item.id)
	return err
}

// enqueueDurable persists only opted-in observers; memory queues are independent.
func (m *Manager) enqueueDurable(ctx context.Context, event Event) error {
	if m.db == nil {
		return errors.New("durable plugin queue unavailable")
	}
	payload, err := json.Marshal(event)
	if err != nil {
		return errors.New("cannot encode plugin event")
	}
	operation, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	tx, err := m.db.BeginTx(operation, nil)
	if err != nil {
		return errors.New("cannot enqueue plugin event")
	}
	defer tx.Rollback()
	for _, name := range m.durableNames {
		_, err = tx.ExecContext(operation, `INSERT INTO plugin_jobs(plugin, peer_id, message_id, destination_id, payload)
   VALUES (?, ?, ?, ?, ?) ON CONFLICT DO NOTHING`, name, event.VKPeerID, event.VKMessageID, event.TelegramChatID, payload)
		if err != nil {
			return errors.New("cannot enqueue plugin event")
		}
	}
	if tx.Commit() != nil {
		return errors.New("cannot commit plugin event")
	}
	for _, w := range m.workers {
		if w.durable {
			select {
			case w.wake <- struct{}{}:
			default:
			}
		}
	}
	return nil
}

// workDurable returns false when a callback may still be running, preventing
// concurrent Close. Pending jobs stay in SQLite when the worker stops.
func (m *Manager) workDurable(ctx context.Context, w *worker, handler Handler) bool {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for ctx.Err() == nil {
		item, err := m.next(ctx, w.name)
		if err == nil {
			var event Event
			if json.Unmarshal(item.payload, &event) != nil || event.Version != 1 {
				m.logger.Warn("plugin queue event is invalid", "plugin", w.name)
				return true
			}
			err, finished := invoke(ctx, m.timeout, func(operation context.Context) error { return handler.Handle(operation, event) })
			if !finished {
				m.logger.Warn("plugin worker timed out or cancelled; pending jobs retained", "plugin", w.name)
				return false
			}
			if err != nil {
				m.logger.Warn("plugin handling failed; retry scheduled", "plugin", w.name)
			}
			if m.finish(ctx, item, err == nil) != nil {
				m.logger.Warn("plugin queue acknowledgement failed", "plugin", w.name)
			} else {
				continue
			}
		} else if !errors.Is(err, sql.ErrNoRows) && ctx.Err() == nil {
			m.logger.Warn("plugin queue read failed", "plugin", w.name)
		}
		select {
		case <-ctx.Done():
			return true
		case <-w.wake:
		case <-ticker.C:
		}
	}
	return true
}
