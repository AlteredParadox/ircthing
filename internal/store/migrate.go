package store

import (
	"database/sql"
	"embed"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Schema changes only ever happen as new numbered files in migrations/ —
// never by editing an applied one. Files are named NNNN_description.sql
// and applied in order, each in its own transaction, recorded in
// schema_migrations.

//go:embed migrations/*.sql
var migrationsFS embed.FS

func migrate(db *sql.DB) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (
		version    INTEGER PRIMARY KEY,
		name       TEXT NOT NULL,
		applied_at TEXT NOT NULL
	) STRICT`); err != nil {
		return fmt.Errorf("store: create schema_migrations: %w", err)
	}

	applied := make(map[int64]bool)
	var maxApplied int64
	rows, err := db.Query(`SELECT version FROM schema_migrations`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var v int64
		if err := rows.Scan(&v); err != nil {
			rows.Close()
			return err
		}
		applied[v] = true
		if v > maxApplied {
			maxApplied = v
		}
	}
	if err := rows.Close(); err != nil {
		return err
	}

	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		return err
	}
	names := make([]string, 0, len(entries))
	var maxKnown int64
	for _, e := range entries {
		names = append(names, e.Name())
		if v, err := migrationVersion(e.Name()); err == nil && v > maxKnown {
			maxKnown = v
		}
	}
	sort.Strings(names) // zero-padded numeric prefixes sort correctly
	if maxApplied > maxKnown {
		return fmt.Errorf("store: database schema version %d is newer than this binary knows (%d) — refusing to open", maxApplied, maxKnown)
	}

	for _, name := range names {
		version, err := migrationVersion(name)
		if err != nil {
			return err
		}
		if applied[version] {
			continue
		}
		if version < maxApplied {
			return fmt.Errorf("store: migration %s is older than already-applied version %d — migrations must only be appended", name, maxApplied)
		}
		sqlText, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			return err
		}
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(string(sqlText)); err != nil {
			tx.Rollback()
			return fmt.Errorf("store: migration %s: %w", name, err)
		}
		if _, err := tx.Exec(`INSERT INTO schema_migrations (version, name, applied_at) VALUES (?, ?, ?)`,
			version, name, time.Now().UTC().Format(time.RFC3339)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("store: migration %s: commit: %w", name, err)
		}
		maxApplied = version
	}
	return nil
}

// migrationVersion extracts the numeric prefix of "NNNN_description.sql".
func migrationVersion(name string) (int64, error) {
	prefix, _, ok := strings.Cut(name, "_")
	if !ok {
		return 0, fmt.Errorf("store: migration file %q is not named NNNN_description.sql", name)
	}
	v, err := strconv.ParseInt(prefix, 10, 64)
	if err != nil || v <= 0 {
		return 0, fmt.Errorf("store: migration file %q has no numeric version prefix", name)
	}
	return v, nil
}
