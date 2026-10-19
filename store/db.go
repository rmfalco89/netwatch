package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

type DB struct {
	*sql.DB
}

func Open(path string) (*DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return nil, fmt.Errorf("create db directory: %w", err)
	}
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	db.SetMaxOpenConns(1)
	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &DB{db}, nil
}

// Schema v2. The v1 tables (usage, usage_hourly, usage_daily, app_groups)
// are left in place untouched: v1 mixed loopback with real network traffic
// and could not tell them apart, so nothing reads them any more.
//
//	traffic        per minute, per label/process, class and direction
//	traffic_hourly the same, rolled up once minute rows age out
//	endpoints      per hour, per label and remote address (off-host only)
//	system         per minute ground truth: "internet" (default routes) and
//	               "if:<name>" interface counters
//	system_hourly  rolled up
//	destinations   per hour, per internet address, all processes together
//	               (measured from host routes; includes short connections)
//	ip_info        who owns an internet address (PTR + ASN), cached
func migrate(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS traffic (
			ts INTEGER NOT NULL, label TEXT NOT NULL, process TEXT NOT NULL,
			class TEXT NOT NULL, dir TEXT NOT NULL,
			bytes_in INTEGER NOT NULL, bytes_out INTEGER NOT NULL,
			PRIMARY KEY (ts, label, process, class, dir)
		) WITHOUT ROWID;
		CREATE TABLE IF NOT EXISTS traffic_hourly (
			ts INTEGER NOT NULL, label TEXT NOT NULL, process TEXT NOT NULL,
			class TEXT NOT NULL, dir TEXT NOT NULL,
			bytes_in INTEGER NOT NULL, bytes_out INTEGER NOT NULL,
			PRIMARY KEY (ts, label, process, class, dir)
		) WITHOUT ROWID;
		CREATE TABLE IF NOT EXISTS endpoints (
			ts INTEGER NOT NULL, label TEXT NOT NULL, remote TEXT NOT NULL,
			port INTEGER NOT NULL, class TEXT NOT NULL, dir TEXT NOT NULL,
			bytes_in INTEGER NOT NULL, bytes_out INTEGER NOT NULL,
			PRIMARY KEY (ts, label, remote, port, class, dir)
		) WITHOUT ROWID;
		CREATE INDEX IF NOT EXISTS idx_endpoints_remote ON endpoints(remote);
		CREATE TABLE IF NOT EXISTS system (
			ts INTEGER NOT NULL, kind TEXT NOT NULL,
			bytes_in INTEGER NOT NULL, bytes_out INTEGER NOT NULL,
			PRIMARY KEY (ts, kind)
		) WITHOUT ROWID;
		CREATE TABLE IF NOT EXISTS system_hourly (
			ts INTEGER NOT NULL, kind TEXT NOT NULL,
			bytes_in INTEGER NOT NULL, bytes_out INTEGER NOT NULL,
			PRIMARY KEY (ts, kind)
		) WITHOUT ROWID;
		CREATE TABLE IF NOT EXISTS destinations (
			ts INTEGER NOT NULL, remote TEXT NOT NULL,
			bytes_in INTEGER NOT NULL, bytes_out INTEGER NOT NULL,
			PRIMARY KEY (ts, remote)
		) WITHOUT ROWID;
		CREATE TABLE IF NOT EXISTS ip_info (
			ip TEXT PRIMARY KEY, ptr TEXT NOT NULL DEFAULT '',
			asn TEXT NOT NULL DEFAULT '', org TEXT NOT NULL DEFAULT '',
			country TEXT NOT NULL DEFAULT '', looked_up INTEGER NOT NULL
		);
	`)
	return err
}
