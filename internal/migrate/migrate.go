// Package migrate applies plain-SQL migration files exactly once, in name
// order, recording what ran in a pgc_migrations table. Files roll the schema
// forward only: undoing a change means writing the next migration, not
// running an old one backwards.
package migrate

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/goloop/pgc/internal/pgwire"
)

// DB is the single connection method migrations need; *pgwire.Conn
// satisfies it. The simple-query protocol executes multi-statement scripts,
// which is exactly what a migration file is.
type DB interface {
	Query(sql string) ([][]pgwire.Value, error)
}

// lockKey is the advisory lock every migrate run takes, so two concurrent
// runs (two CI jobs, say) queue instead of racing. 7366499 is "pgc" read as
// a big-endian integer.
const lockKey = 7366499

// noTxMarker on the first line of a file opts it out of the transaction
// wrapper, for statements PostgreSQL refuses to run inside one
// (CREATE INDEX CONCURRENTLY and friends).
const noTxMarker = "-- pgc: no-transaction"

// Migration is one migrations-directory file and its state.
type Migration struct {
	Name      string // file name, e.g. 001_init.sql
	Applied   bool
	AppliedAt string // server timestamp, when applied
}

// Result reports what a run did.
type Result struct {
	Applied  []string // files applied by this run, in order
	Warnings []string
}

// Run applies every pending migration file from dir, each inside its own
// transaction together with its bookkeeping row, holding an advisory lock
// for the whole run.
func Run(db DB, dir string) (*Result, error) {
	res := &Result{}

	if _, err := db.Query(fmt.Sprintf(
		"SELECT pg_advisory_lock(%d)", lockKey)); err != nil {
		return nil, fmt.Errorf("migrate: acquire lock: %w", err)
	}
	defer db.Query(fmt.Sprintf("SELECT pg_advisory_unlock(%d)", lockKey))

	if err := ensureTable(db); err != nil {
		return nil, err
	}
	files, applied, err := load(db, dir)
	if err != nil {
		return nil, err
	}

	lastApplied := ""
	for name := range applied {
		if name > lastApplied {
			lastApplied = name
		}
	}

	for _, name := range files {
		path := filepath.Join(dir, name)
		content, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("migrate: %w", err)
		}
		hash := sha256hex(content)

		if prev, ok := applied[name]; ok {
			if prev != hash {
				res.Warnings = append(res.Warnings, fmt.Sprintf(
					"%s changed after it was applied; applied migrations "+
						"should never be edited - write a new one", name))
			}
			continue
		}
		if name < lastApplied {
			res.Warnings = append(res.Warnings, fmt.Sprintf(
				"%s sorts before already-applied %s; applying it now "+
					"(out-of-order, usually a merged branch)", name, lastApplied))
		}

		if err := apply(db, name, string(content), hash); err != nil {
			return res, err
		}
		res.Applied = append(res.Applied, name)
	}
	return res, nil
}

// Status lists every migration file and recorded row, without changing
// anything.
func Status(db DB, dir string) ([]Migration, []string, error) {
	if err := ensureTable(db); err != nil {
		return nil, nil, err
	}
	files, applied, err := load(db, dir)
	if err != nil {
		return nil, nil, err
	}

	rows, err := db.Query(
		"SELECT name, to_char(applied_at, 'YYYY-MM-DD HH24:MI') " +
			"FROM pgc_migrations ORDER BY name")
	if err != nil {
		return nil, nil, fmt.Errorf("migrate: %w", err)
	}
	when := map[string]string{}
	for _, row := range rows {
		when[row[0].S] = row[1].S
	}

	var list []Migration
	var warnings []string
	for _, name := range files {
		_, ok := applied[name]
		list = append(list, Migration{
			Name: name, Applied: ok, AppliedAt: when[name],
		})
	}
	for name := range applied {
		if !contains(files, name) {
			warnings = append(warnings, fmt.Sprintf(
				"%s is recorded as applied but the file is gone", name))
		}
	}
	sort.Strings(warnings)
	return list, warnings, nil
}

// apply runs one file. The default wraps the script and its bookkeeping row
// in a single transaction; the no-transaction marker switches to
// statement-by-statement autocommit for DDL that refuses transactions.
func apply(db DB, name, content, hash string) error {
	record := fmt.Sprintf(
		"INSERT INTO pgc_migrations (name, hash) VALUES ('%s', '%s')",
		sqlEscape(name), hash)

	firstLine, _, _ := strings.Cut(strings.TrimSpace(content), "\n")
	if strings.TrimSpace(firstLine) == noTxMarker {
		for i, stmt := range splitStatements(content) {
			if _, err := db.Query(stmt); err != nil {
				return fmt.Errorf(
					"migrate: %s, statement %d: %w (the file is marked "+
						"no-transaction, earlier statements stay applied)",
					name, i+1, err)
			}
		}
		if _, err := db.Query(record); err != nil {
			return fmt.Errorf("migrate: %s: record: %w", name, err)
		}
		return nil
	}

	if _, err := db.Query("BEGIN"); err != nil {
		return fmt.Errorf("migrate: %s: %w", name, err)
	}
	if _, err := db.Query(content); err != nil {
		db.Query("ROLLBACK")
		return fmt.Errorf("migrate: %s: %w", name, err)
	}
	if _, err := db.Query(record); err != nil {
		db.Query("ROLLBACK")
		return fmt.Errorf("migrate: %s: record: %w", name, err)
	}
	if _, err := db.Query("COMMIT"); err != nil {
		return fmt.Errorf("migrate: %s: %w", name, err)
	}
	return nil
}

// ensureTable creates the bookkeeping table on first contact. It is created
// unqualified - in the first schema of the connection's search_path,
// normally public.
func ensureTable(db DB) error {
	_, err := db.Query(`CREATE TABLE IF NOT EXISTS pgc_migrations (
    name       text PRIMARY KEY,
    hash       text NOT NULL,
    applied_at timestamptz NOT NULL DEFAULT now()
)`)
	if err != nil {
		return fmt.Errorf("migrate: bookkeeping table: %w", err)
	}
	return nil
}

// load returns the sorted migration file names and the applied name-to-hash
// map.
func load(db DB, dir string) ([]string, map[string]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil, fmt.Errorf("migrate: %w", err)
	}
	var files []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			files = append(files, e.Name())
		}
	}
	sort.Strings(files)

	rows, err := db.Query("SELECT name, hash FROM pgc_migrations")
	if err != nil {
		return nil, nil, fmt.Errorf("migrate: %w", err)
	}
	applied := map[string]string{}
	for _, row := range rows {
		applied[row[0].S] = row[1].S
	}
	return files, applied, nil
}

func sha256hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// sqlEscape doubles single quotes; names come from the filesystem, hashes
// are hex, but quoting stays correct regardless.
func sqlEscape(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
