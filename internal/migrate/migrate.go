// Package migrate applies plain-SQL migration files exactly once, in name
// order, recording what ran in a pgc_migrations table. Files roll the schema
// forward only: undoing a change means writing the next migration, not
// running an old one backwards.
package migrate

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/goloop/pgc/internal/pgwire"
)

// DB is the connection migrations need; *pgwire.Conn satisfies it. Query
// reads the history, Exec runs scripts without keeping their rows, and
// TxStatus reports the transaction status the last request left behind, so a
// file that ends pgc's transaction on its own is caught.
type DB interface {
	Query(sql string) ([][]pgwire.Value, error)
	Exec(sql string) error
	TxStatus() byte
}

// lockKey is the advisory lock every run takes, so two concurrent runs (two
// CI jobs, say) queue instead of racing. 7366499 is "pgc" read as a
// big-endian integer.
const lockKey = 7366499

// The lock functions are called fully qualified and with a bigint argument:
// an unqualified pg_advisory_lock(7366499) would resolve to any
// pg_advisory_lock(integer) a schema on the search_path defines - an exact
// match beats the built-in bigint one - and a decoy could let two runs in at
// once.
var (
	lockSQL   = fmt.Sprintf("SELECT pg_catalog.pg_advisory_lock(%d::bigint)", lockKey)
	unlockSQL = fmt.Sprintf("SELECT pg_catalog.pg_advisory_unlock(%d::bigint)", lockKey)
)

// migrationsTable is the fully qualified bookkeeping table. Qualifying it
// keeps a decoy pgc_migrations in another schema from shadowing the real
// migration history.
const migrationsTable = "public.pgc_migrations"

// baselineSQL is the session every file starts from: settings, role and
// search_path as the connection opened, with unqualified names resolving to
// public. It runs before each file, so a SET search_path or SET ROLE in one
// file does not carry over into the next.
const baselineSQL = "RESET ALL; RESET ROLE; SET search_path TO public"

// noTxMarker on the first line of a file opts it out of the transaction
// wrapper, for statements PostgreSQL refuses to run inside one
// (CREATE INDEX CONCURRENTLY and friends).
const noTxMarker = "-- pgc: no-transaction"

// adoptedHash is the hash older documentation told people to record for a
// file they marked applied by hand. Such a row carries no checksum to
// compare, and is accepted as it is.
const adoptedHash = "adopted"

// The states a history row can be in. A transactional file is only ever
// recorded as applied, in the same transaction as its changes. A
// no-transaction file is recorded as started before its first statement and
// as applied after its last, so a run that stops half way leaves a mark the
// next run will not walk past.
const (
	stateApplied = "applied"
	stateStarted = "started"
	stateFailed  = "failed"
)

// Options tune a run.
type Options struct {
	// AllowDrift applies pending files even when the history disagrees
	// with the directory - an applied file edited or deleted since - and
	// reports the disagreement as warnings instead of refusing.
	AllowDrift bool

	// LockTimeout bounds the wait for another run's lock; zero waits for
	// as long as it takes.
	LockTimeout time.Duration
}

// Migration is one migration's state: a file, a history row, or both.
type Migration struct {
	Name      string // file name, e.g. 001_init.sql
	State     string // pending, applied, changed, missing, started or failed
	AppliedAt string // server timestamp, when recorded
}

// Problem reports whether the state needs a person to look at it.
func (m Migration) Problem() bool {
	switch m.State {
	case "pending", stateApplied:
		return false
	}
	return true
}

// Result reports what a run did.
type Result struct {
	Applied  []string // files applied by this run, in order
	Warnings []string
}

// file is one migration file, read once: what is hashed is what runs.
type file struct {
	name    string
	content string
	hash    string
	noTx    bool
}

// row is one history row.
type row struct {
	hash, state, at string
}

// Run applies every pending migration file from dir, each inside its own
// transaction together with its history row, holding an advisory lock for
// the whole run.
//
// Nothing runs until the whole directory has been read and checked, and the
// history has been compared with it under the lock: a file that would end
// pgc's transaction, a half-finished no-transaction file, and - unless
// opts.AllowDrift - an applied file that changed or disappeared all stop the
// run before the first pending file.
func Run(db DB, dir string, opts Options) (res *Result, err error) {
	files, err := readDir(dir)
	if err != nil {
		return nil, err
	}
	for _, f := range files {
		if err := checkFile(f); err != nil {
			return nil, err
		}
	}

	if err := lock(db, opts.LockTimeout); err != nil {
		return nil, err
	}
	defer func() {
		if uerr := unlock(db); uerr != nil && err == nil && res != nil {
			res.Warnings = append(res.Warnings, uerr.Error())
		}
	}()

	res = &Result{}
	if err := db.Exec(baselineSQL); err != nil {
		return nil, fmt.Errorf("migrate: reset session: %w", err)
	}
	if err := ensureTable(db); err != nil {
		return nil, err
	}
	history, err := readHistory(db)
	if err != nil {
		return nil, err
	}

	list := compare(files, history)
	var drift []string
	for _, m := range list {
		switch m.State {
		case stateStarted, stateFailed:
			return res, dirtyError(m)
		case "changed":
			drift = append(drift, fmt.Sprintf("%s changed after it was "+
				"applied; applied migrations are never edited - write a new one",
				m.Name))
		case "missing":
			drift = append(drift, fmt.Sprintf("%s is recorded as applied but "+
				"the file is gone", m.Name))
		}
	}
	if len(drift) > 0 && !opts.AllowDrift {
		return res, fmt.Errorf("migrate: the history does not match the "+
			"directory, nothing was applied:\n  %s\n(put the files back as "+
			"they were applied, or pass -allow-drift to apply pending files "+
			"anyway)", strings.Join(drift, "\n  "))
	}
	res.Warnings = append(res.Warnings, drift...)

	lastApplied := ""
	for name := range history {
		if name > lastApplied {
			lastApplied = name
		}
	}
	for _, f := range files {
		if _, ok := history[f.name]; ok {
			continue
		}
		if f.name < lastApplied {
			res.Warnings = append(res.Warnings, fmt.Sprintf(
				"%s sorts before already-applied %s; applying it now "+
					"(out-of-order, usually a merged branch)", f.name, lastApplied))
		}
		if err := db.Exec(baselineSQL); err != nil {
			return res, fmt.Errorf("migrate: reset session: %w", err)
		}
		if err := apply(db, f); err != nil {
			return res, err
		}
		res.Applied = append(res.Applied, f.name)
	}
	return res, nil
}

// dirtyError explains a half-finished no-transaction file and the way out.
func dirtyError(m Migration) error {
	how := "stopped part way (the run was interrupted)"
	if m.State == stateFailed {
		how = "failed part way"
	}
	return fmt.Errorf("migrate: %s is a no-transaction migration that %s; "+
		"its earlier statements may have taken effect. Check the database, "+
		"then record the outcome: pgc migrate resolve %s applied (it is "+
		"complete now) or pgc migrate resolve %s retry (run it again from "+
		"the top)", m.Name, how, m.Name, m.Name)
}

// Status lists every migration file and history row. It only reads: on a
// database pgc has never migrated every file is pending, and no table is
// created.
func Status(db DB, dir string) ([]Migration, error) {
	files, err := readDir(dir)
	if err != nil {
		return nil, err
	}
	exists, err := tableExists(db)
	if err != nil {
		return nil, err
	}
	history := map[string]row{}
	if exists {
		if history, err = readHistory(db); err != nil {
			return nil, err
		}
	}
	return compare(files, history), nil
}

// Resolve records the outcome of a no-transaction file left started or
// failed, after a person has checked the database: "applied" marks it
// complete with the file's current checksum, "retry" removes the mark so the
// next run starts it again from the top.
func Resolve(db DB, dir, name, outcome string) error {
	if outcome != "applied" && outcome != "retry" {
		return fmt.Errorf("migrate: resolve %s: the outcome is applied or "+
			"retry, not %q", name, outcome)
	}
	if err := lock(db, 0); err != nil {
		return err
	}
	defer unlock(db)

	if err := ensureTable(db); err != nil {
		return err
	}
	history, err := readHistory(db)
	if err != nil {
		return err
	}
	r, ok := history[name]
	if !ok || r.state == stateApplied {
		return fmt.Errorf("migrate: resolve %s: there is no started or "+
			"failed record of it", name)
	}

	if outcome == "retry" {
		err = db.Exec(fmt.Sprintf("DELETE FROM %s WHERE name = %s",
			migrationsTable, quote(name)))
	} else {
		var content []byte
		content, err = os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return fmt.Errorf("migrate: resolve: %w", err)
		}
		err = db.Exec(fmt.Sprintf("UPDATE %s SET state = '%s', hash = %s, "+
			"applied_at = now() WHERE name = %s", migrationsTable,
			stateApplied, quote(sha256hex(content)), quote(name)))
	}
	if err != nil {
		return fmt.Errorf("migrate: resolve %s: %w", name, err)
	}
	return nil
}

// Baseline records every file up to and including through as applied,
// without running any of them, for a database whose schema those files
// already describe - the way onto pgc migrate for an existing database. It
// only writes to an empty history, and records the files' real checksums.
func Baseline(db DB, dir, through string) ([]string, error) {
	files, err := readDir(dir)
	if err != nil {
		return nil, err
	}
	var names []string
	var values []string
	for _, f := range files {
		if f.name > through {
			break
		}
		names = append(names, f.name)
		values = append(values, fmt.Sprintf("(%s, %s, '%s')",
			quote(f.name), quote(f.hash), stateApplied))
	}
	if len(names) == 0 || names[len(names)-1] != through {
		return nil, fmt.Errorf("migrate: baseline: %s is not a file of %s",
			through, dir)
	}

	if err := lock(db, 0); err != nil {
		return nil, err
	}
	defer unlock(db)

	if err := ensureTable(db); err != nil {
		return nil, err
	}
	history, err := readHistory(db)
	if err != nil {
		return nil, err
	}
	if len(history) > 0 {
		return nil, fmt.Errorf("migrate: baseline: the history already has " +
			"entries; a baseline only starts an empty one")
	}
	if err := db.Exec(fmt.Sprintf("INSERT INTO %s (name, hash, state) VALUES %s",
		migrationsTable, strings.Join(values, ", "))); err != nil {
		return nil, fmt.Errorf("migrate: baseline: %w", err)
	}
	return names, nil
}

// compare lines the files up with the history.
func compare(files []file, history map[string]row) []Migration {
	var list []Migration
	seen := map[string]bool{}
	for _, f := range files {
		seen[f.name] = true
		r, ok := history[f.name]
		m := Migration{Name: f.name, State: "pending"}
		if ok {
			m.AppliedAt = r.at
			switch {
			case r.state != stateApplied:
				m.State = r.state
			case r.hash != f.hash && r.hash != adoptedHash:
				m.State = "changed"
			default:
				m.State = stateApplied
			}
		}
		list = append(list, m)
	}
	var missing []Migration
	for name, r := range history {
		if !seen[name] {
			missing = append(missing, Migration{Name: name, State: "missing", AppliedAt: r.at})
		}
	}
	sort.Slice(missing, func(i, j int) bool { return missing[i].Name < missing[j].Name })
	return append(list, missing...)
}

// apply runs one file. The default wraps the script and its history row in
// a single transaction; the no-transaction marker switches to
// statement-by-statement autocommit for DDL that refuses transactions.
func apply(db DB, f file) error {
	if f.noTx {
		return applyNoTx(db, f)
	}

	if err := db.Exec("BEGIN"); err != nil {
		return fmt.Errorf("migrate: %s: %w", f.name, err)
	}
	if err := db.Exec(f.content); err != nil {
		db.Exec("ROLLBACK")
		return fmt.Errorf("migrate: %s: %w", f.name, err)
	}
	// checkFile refuses transaction control before anything runs; this is
	// the backstop for whatever it could not see (a procedure that
	// commits, say): the file must leave pgc's transaction open.
	if db.TxStatus() != 'T' {
		db.Exec("ROLLBACK")
		return fmt.Errorf("migrate: %s ended pgc's transaction itself, so it "+
			"was not recorded; whatever it did before that may have been "+
			"committed - check the database", f.name)
	}
	if err := db.Exec(record(f.name, f.hash, stateApplied)); err != nil {
		db.Exec("ROLLBACK")
		return fmt.Errorf("migrate: %s: record: %w", f.name, err)
	}
	if err := db.Exec("COMMIT"); err != nil {
		var srv *pgwire.ServerError
		if errors.As(err, &srv) {
			return fmt.Errorf("migrate: %s: commit: %w", f.name, err)
		}
		return fmt.Errorf("migrate: %s: the connection failed during COMMIT, "+
			"so whether it took effect is unknown; pgc migrate status will "+
			"show it as applied if it did: %w", f.name, err)
	}
	return nil
}

// applyNoTx runs a no-transaction file one statement at a time, bracketed by
// a started mark and the applied record.
func applyNoTx(db DB, f file) error {
	if err := db.Exec(record(f.name, f.hash, stateStarted)); err != nil {
		return fmt.Errorf("migrate: %s: record: %w", f.name, err)
	}
	fail := func(err error) error {
		db.Exec(fmt.Sprintf("UPDATE %s SET state = '%s' WHERE name = %s",
			migrationsTable, stateFailed, quote(f.name)))
		return err
	}
	for i, stmt := range splitStatements(f.content) {
		if err := db.Exec(stmt); err != nil {
			if db.TxStatus() != 'I' {
				db.Exec("ROLLBACK")
			}
			return fail(fmt.Errorf("migrate: %s, statement %d: %w (the file "+
				"is marked no-transaction: earlier statements stay applied, "+
				"and the file is recorded as failed until pgc migrate "+
				"resolve says otherwise)", f.name, i+1, err))
		}
	}
	if db.TxStatus() != 'I' {
		db.Exec("ROLLBACK")
		return fail(fmt.Errorf("migrate: %s left a transaction open; its "+
			"last transaction was rolled back", f.name))
	}
	if err := db.Exec(fmt.Sprintf("UPDATE %s SET state = '%s', "+
		"applied_at = now() WHERE name = %s",
		migrationsTable, stateApplied, quote(f.name))); err != nil {
		return fmt.Errorf("migrate: %s: record: %w", f.name, err)
	}
	return nil
}

// record renders the INSERT of one history row.
func record(name, hash, state string) string {
	return fmt.Sprintf("INSERT INTO %s (name, hash, state) VALUES (%s, %s, '%s')",
		migrationsTable, quote(name), quote(hash), state)
}

// lock takes the run's advisory lock, waiting at most timeout (0: no
// limit). The wait is one blocking statement bounded by lock_timeout, so a
// Cancel from another goroutine interrupts it as it would any statement.
func lock(db DB, timeout time.Duration) error {
	if timeout > 0 {
		ms := timeout.Milliseconds()
		if ms < 1 {
			ms = 1
		}
		if err := db.Exec(fmt.Sprintf("SET lock_timeout = %d", ms)); err != nil {
			return fmt.Errorf("migrate: acquire lock: %w", err)
		}
		defer db.Exec("RESET lock_timeout")
	}
	if _, err := db.Query(lockSQL); err != nil {
		var srv *pgwire.ServerError
		if errors.As(err, &srv) && srv.Code == "55P03" { // lock_not_available
			return fmt.Errorf("migrate: another migration run has held the "+
				"lock for more than %v; giving up", timeout)
		}
		return fmt.Errorf("migrate: acquire lock: %w", err)
	}
	return nil
}

// unlock releases the advisory lock and reports when it was not held.
func unlock(db DB) error {
	rows, err := db.Query(unlockSQL)
	if err != nil {
		return fmt.Errorf("migrate: release lock: %w", err)
	}
	if len(rows) != 1 || rows[0][0].S != "t" {
		return fmt.Errorf("migrate: the migration lock was not held at the " +
			"end of the run")
	}
	return nil
}

// ensureTable creates the history table on first contact, and adds the state
// column to a table an older pgc created.
func ensureTable(db DB) error {
	if err := db.Exec(`CREATE TABLE IF NOT EXISTS ` + migrationsTable + ` (
    name       text PRIMARY KEY,
    hash       text NOT NULL,
    state      text NOT NULL DEFAULT 'applied',
    applied_at timestamptz NOT NULL DEFAULT now()
)`); err != nil {
		return fmt.Errorf("migrate: history table: %w", err)
	}
	has, err := hasStateColumn(db)
	if err != nil {
		return err
	}
	if !has {
		if err := db.Exec(`ALTER TABLE ` + migrationsTable +
			` ADD COLUMN state text NOT NULL DEFAULT 'applied'`); err != nil {
			return fmt.Errorf("migrate: history table: %w", err)
		}
	}
	return nil
}

// tableExists reports whether the history table is there.
func tableExists(db DB) (bool, error) {
	rows, err := db.Query("SELECT pg_catalog.to_regclass('" + migrationsTable +
		"') IS NOT NULL")
	if err != nil {
		return false, fmt.Errorf("migrate: %w", err)
	}
	return len(rows) == 1 && rows[0][0].S == "t", nil
}

// hasStateColumn reports whether the history table has the state column
// (tables created before it existed do not).
func hasStateColumn(db DB) (bool, error) {
	rows, err := db.Query("SELECT count(*) FROM pg_catalog.pg_attribute " +
		"WHERE attrelid = '" + migrationsTable + "'::regclass " +
		"AND attname = 'state' AND NOT attisdropped")
	if err != nil {
		return false, fmt.Errorf("migrate: %w", err)
	}
	return len(rows) == 1 && rows[0][0].S == "1", nil
}

// readHistory returns the history rows by name.
func readHistory(db DB) (map[string]row, error) {
	has, err := hasStateColumn(db)
	if err != nil {
		return nil, err
	}
	state := "'applied'"
	if has {
		state = "state"
	}
	rows, err := db.Query("SELECT name, hash, " + state + ", " +
		"to_char(applied_at, 'YYYY-MM-DD HH24:MI') FROM " + migrationsTable)
	if err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	history := make(map[string]row, len(rows))
	for _, r := range rows {
		if len(r) != 4 {
			return nil, fmt.Errorf("migrate: unexpected history row")
		}
		history[r[0].S] = row{hash: r[1].S, state: r[2].S, at: r[3].S}
	}
	return history, nil
}

// readDir reads every .sql file of dir, in name order.
func readDir(dir string) ([]file, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	var files []file
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		content, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("migrate: %w", err)
		}
		firstLine, _, _ := strings.Cut(strings.TrimSpace(string(content)), "\n")
		files = append(files, file{
			name:    e.Name(),
			content: string(content),
			hash:    sha256hex(content),
			noTx:    strings.TrimSpace(firstLine) == noTxMarker,
		})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].name < files[j].name })
	return files, nil
}

// checkFile refuses a transactional file that controls the transaction
// itself. pgc runs the file inside its own transaction, together with the
// history row; a COMMIT in the file would make its first half permanent
// before the row is written, a ROLLBACK would undo the file and leave the row
// to be recorded on its own.
func checkFile(f file) error {
	if f.noTx {
		return nil
	}
	for i, stmt := range splitStatements(f.content) {
		if word := transactionControl(stmt); word != "" {
			return fmt.Errorf("migrate: %s, statement %d: %s - each file "+
				"already runs in a transaction of its own; remove it, or mark "+
				"the file %q to run it statement by statement",
				f.name, i+1, word, noTxMarker)
		}
	}
	return nil
}

func sha256hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// quote renders s as a SQL string literal. Names come from the filesystem and
// hashes are hex, but quoting stays correct regardless.
func quote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}
