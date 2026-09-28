package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// Selection is the persisted global choice. An empty ProviderID and ProxyID mean no current proxy.
type Selection struct {
	ProviderID   string
	ProxyID      string
	AutoSwitch   bool
	SelectedAt   time.Time
	SwitchReason string
}

type Health struct {
	ProviderID           string
	ProxyID              string
	Status               string
	ConsecutiveSuccesses int
	ConsecutiveFailures  int
	LastCheckedAt        time.Time
	LastResult           string
	LastError            string
	NextCheckAt          time.Time
	BackoffStage         int
}

type ProxyKey struct {
	ProviderID string
	ProxyID    string
}

type Store struct{ db *sql.DB }

func Open(ctx context.Context, path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	for _, pragma := range []string{"PRAGMA busy_timeout = 5000", "PRAGMA journal_mode = WAL", "PRAGMA foreign_keys = ON"} {
		if _, err := db.ExecContext(ctx, pragma); err != nil {
			db.Close()
			return nil, err
		}
	}
	s := &Store{db: db}
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var version int
	if err := tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version > 1 {
		return fmt.Errorf("database schema version %d is newer than this binary", version)
	}
	if version == 0 {
		for _, statement := range []string{
			`CREATE TABLE selection (
				id INTEGER PRIMARY KEY CHECK (id = 1),
				provider_id TEXT NOT NULL DEFAULT '', proxy_id TEXT NOT NULL DEFAULT '',
				auto_switch INTEGER NOT NULL DEFAULT 0 CHECK (auto_switch IN (0, 1)),
				selected_at TEXT NOT NULL DEFAULT '', switch_reason TEXT NOT NULL DEFAULT ''
			)`,
			`INSERT INTO selection (id) VALUES (1)`,
			`CREATE TABLE health (
				provider_id TEXT NOT NULL, proxy_id TEXT NOT NULL,
				status TEXT NOT NULL CHECK (status IN ('unknown', 'healthy', 'suspect', 'unavailable')),
				consecutive_successes INTEGER NOT NULL DEFAULT 0,
				consecutive_failures INTEGER NOT NULL DEFAULT 0,
				last_checked_at TEXT NOT NULL DEFAULT '', last_result TEXT NOT NULL DEFAULT '',
				last_error TEXT NOT NULL DEFAULT '', next_check_at TEXT NOT NULL DEFAULT '',
				backoff_stage INTEGER NOT NULL DEFAULT 0,
				PRIMARY KEY (provider_id, proxy_id)
			)`,
			`PRAGMA user_version = 1`,
		} {
			if _, err := tx.ExecContext(ctx, statement); err != nil {
				return fmt.Errorf("migrate database: %w", err)
			}
		}
	}
	return tx.Commit()
}

func (s *Store) LoadSelection(ctx context.Context) (Selection, error) {
	var v Selection
	var enabled int
	var selectedAt string
	err := s.db.QueryRowContext(ctx, `SELECT provider_id, proxy_id, auto_switch, selected_at, switch_reason FROM selection WHERE id = 1`).Scan(&v.ProviderID, &v.ProxyID, &enabled, &selectedAt, &v.SwitchReason)
	if err != nil {
		return Selection{}, err
	}
	v.AutoSwitch = enabled == 1
	v.SelectedAt, err = parseTime(selectedAt)
	return v, err
}

func (s *Store) SaveSelection(ctx context.Context, v Selection) error {
	if (v.ProviderID == "") != (v.ProxyID == "") {
		return errors.New("provider and proxy IDs must both be set or empty")
	}
	_, err := s.db.ExecContext(ctx, `UPDATE selection SET provider_id = ?, proxy_id = ?, auto_switch = ?, selected_at = ?, switch_reason = ? WHERE id = 1`, v.ProviderID, v.ProxyID, v.AutoSwitch, formatTime(v.SelectedAt), v.SwitchReason)
	return err
}

func (s *Store) LoadHealth(ctx context.Context) ([]Health, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT provider_id, proxy_id, status, consecutive_successes, consecutive_failures, last_checked_at, last_result, last_error, next_check_at, backoff_stage FROM health ORDER BY provider_id, proxy_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []Health
	for rows.Next() {
		var h Health
		var checked, next string
		if err := rows.Scan(&h.ProviderID, &h.ProxyID, &h.Status, &h.ConsecutiveSuccesses, &h.ConsecutiveFailures, &checked, &h.LastResult, &h.LastError, &next, &h.BackoffStage); err != nil {
			return nil, err
		}
		if h.LastCheckedAt, err = parseTime(checked); err != nil {
			return nil, err
		}
		if h.NextCheckAt, err = parseTime(next); err != nil {
			return nil, err
		}
		result = append(result, h)
	}
	return result, rows.Err()
}

func (s *Store) SaveHealth(ctx context.Context, h Health) error {
	if h.ProviderID == "" || h.ProxyID == "" || h.ConsecutiveFailures < 0 || h.ConsecutiveSuccesses < 0 || h.BackoffStage < 0 {
		return errors.New("invalid health record")
	}
	switch h.Status {
	case "unknown", "healthy", "suspect", "unavailable":
	default:
		return errors.New("invalid health status")
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO health (provider_id, proxy_id, status, consecutive_successes, consecutive_failures, last_checked_at, last_result, last_error, next_check_at, backoff_stage)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(provider_id, proxy_id) DO UPDATE SET status=excluded.status, consecutive_successes=excluded.consecutive_successes, consecutive_failures=excluded.consecutive_failures,
		last_checked_at=excluded.last_checked_at, last_result=excluded.last_result, last_error=excluded.last_error, next_check_at=excluded.next_check_at, backoff_stage=excluded.backoff_stage`,
		h.ProviderID, h.ProxyID, h.Status, h.ConsecutiveSuccesses, h.ConsecutiveFailures, formatTime(h.LastCheckedAt), h.LastResult, h.LastError, formatTime(h.NextCheckAt), h.BackoffStage)
	return err
}

// ForgetHealth removes stale check results after connection details change.
// The next health scheduler run treats these proxies as unknown and due now.
func (s *Store) ForgetHealth(ctx context.Context, keys []ProxyKey) error {
	if len(keys) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, key := range keys {
		if _, err := tx.ExecContext(ctx, `DELETE FROM health WHERE provider_id = ? AND proxy_id = ?`, key.ProviderID, key.ProxyID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func formatTime(v time.Time) string {
	if v.IsZero() {
		return ""
	}
	return v.UTC().Format(time.RFC3339Nano)
}

func parseTime(v string) (time.Time, error) {
	if v == "" {
		return time.Time{}, nil
	}
	return time.Parse(time.RFC3339Nano, v)
}
