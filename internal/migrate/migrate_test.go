package migrate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goloop/pgc/internal/pgwire"
)

// fakeDB records every executed script and serves the history table from a
// map. It tracks the transaction status the way the server reports it.
type fakeDB struct {
	scripts []string
	applied map[string]string // name -> hash
	states  map[string]string // name -> state, when not applied
	failOn  string            // substring that makes a request fail
	noTable bool              // the history table does not exist
	tx      byte
}

func v(s string) pgwire.Value { return pgwire.Value{S: s, Valid: true} }

func (f *fakeDB) Query(sql string) ([][]pgwire.Value, error) {
	f.scripts = append(f.scripts, sql)
	if f.failOn != "" && strings.Contains(sql, f.failOn) {
		return nil, &pgwire.ServerError{Severity: "ERROR", Code: "42601", Message: "boom"}
	}
	switch {
	case strings.Contains(sql, "to_regclass"):
		if f.noTable {
			return [][]pgwire.Value{{v("f")}}, nil
		}
		return [][]pgwire.Value{{v("t")}}, nil
	case strings.Contains(sql, "pg_attribute"):
		return [][]pgwire.Value{{v("1")}}, nil
	case strings.Contains(sql, "advisory"):
		return [][]pgwire.Value{{v("t")}}, nil
	case strings.HasPrefix(sql, "SELECT name, hash"):
		var rows [][]pgwire.Value
		for name, hash := range f.applied {
			state := "applied"
			if s := f.states[name]; s != "" {
				state = s
			}
			rows = append(rows, []pgwire.Value{v(name), v(hash), v(state), v("2026-07-11 10:00")})
		}
		return rows, nil
	}
	return nil, nil
}

func (f *fakeDB) Exec(sql string) error {
	_, err := f.Query(sql)
	switch {
	case sql == "BEGIN":
		f.tx = 'T'
	case sql == "COMMIT" || sql == "ROLLBACK":
		f.tx = 'I'
	case err != nil && f.tx == 'T':
		f.tx = 'E'
	}
	return err
}

func (f *fakeDB) TxStatus() byte {
	if f.tx == 0 {
		return 'I'
	}
	return f.tx
}

func (f *fakeDB) has(sub string) bool {
	for _, s := range f.scripts {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

func writeMigrations(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestRunAppliesPendingInOrder(t *testing.T) {
	dir := writeMigrations(t, map[string]string{
		"001_a.sql": "CREATE TABLE a (id int);",
		"002_b.sql": "CREATE TABLE b (id int);",
	})
	db := &fakeDB{applied: map[string]string{}}

	res, err := Run(db, dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Applied) != 2 || res.Applied[0] != "001_a.sql" || res.Applied[1] != "002_b.sql" {
		t.Fatalf("applied = %v", res.Applied)
	}
	// Lock taken and released, table ensured, transactions used.
	for _, want := range []string{
		"pg_catalog.pg_advisory_lock(7366499::bigint)",
		"pg_catalog.pg_advisory_unlock(7366499::bigint)",
		"CREATE TABLE IF NOT EXISTS public.pgc_migrations",
		"BEGIN", "COMMIT", "RESET ALL; RESET ROLE; SET search_path TO public",
		"INSERT INTO public.pgc_migrations (name, hash, state) VALUES ('001_a.sql'",
	} {
		if !db.has(want) {
			t.Errorf("missing %q in executed scripts:\n%s", want,
				strings.Join(db.scripts, "\n---\n"))
		}
	}
}

// An applied file that changed or disappeared stops the run before anything
// pending is applied, unless drift is explicitly allowed.
func TestRunRefusesDrift(t *testing.T) {
	dir := writeMigrations(t, map[string]string{
		"001_a.sql": "CREATE TABLE a (id int);",
		"002_b.sql": "CREATE TABLE b (id int);",
	})
	for name, applied := range map[string]map[string]string{
		"changed": {"001_a.sql": "not-the-real-hash"},
		"missing": {
			"001_a.sql":    sha256hex([]byte("CREATE TABLE a (id int);")),
			"000_gone.sql": "x",
		},
	} {
		t.Run(name, func(t *testing.T) {
			db := &fakeDB{applied: applied}
			res, err := Run(db, dir, Options{})
			if err == nil || !strings.Contains(err.Error(), "does not match") {
				t.Fatalf("err = %v, want the drift refused", err)
			}
			if len(res.Applied) != 0 || db.has("CREATE TABLE b") {
				t.Fatal("a pending file ran despite the drift")
			}

			db = &fakeDB{applied: applied}
			res, err = Run(db, dir, Options{AllowDrift: true})
			if err != nil {
				t.Fatal(err)
			}
			if len(res.Applied) == 0 || len(res.Warnings) == 0 {
				t.Fatalf("res = %+v, want pending applied with a warning", res)
			}
		})
	}
}

// A row recorded by hand as "adopted", as older documentation advised, has no
// checksum to compare and is accepted.
func TestRunAcceptsAdoptedRows(t *testing.T) {
	dir := writeMigrations(t, map[string]string{"001_a.sql": "SELECT 1;"})
	db := &fakeDB{applied: map[string]string{"001_a.sql": "adopted"}}
	if _, err := Run(db, dir, Options{}); err != nil {
		t.Fatal(err)
	}
}

// A no-transaction file left started or failed blocks every later run until
// a person resolves it.
func TestRunStopsAtAHalfFinishedFile(t *testing.T) {
	dir := writeMigrations(t, map[string]string{
		"001_a.sql": "-- pgc: no-transaction\nSELECT 1;",
		"002_b.sql": "CREATE TABLE b (id int);",
	})
	for _, state := range []string{"started", "failed"} {
		db := &fakeDB{
			applied: map[string]string{"001_a.sql": "h"},
			states:  map[string]string{"001_a.sql": state},
		}
		_, err := Run(db, dir, Options{AllowDrift: true})
		if err == nil || !strings.Contains(err.Error(), "resolve") {
			t.Fatalf("%s: err = %v, want a pointer to resolve", state, err)
		}
		if db.has("CREATE TABLE b") {
			t.Fatalf("%s: a later file ran", state)
		}
	}
}

// Transaction control in a transactional file is refused before anything
// runs: a ROLLBACK would leave the history row recorded for a file that did
// nothing, a COMMIT would make half a file permanent.
func TestRunRefusesTransactionControl(t *testing.T) {
	for _, content := range []string{
		"CREATE TABLE a (id int); ROLLBACK;",
		"CREATE TABLE a (id int);\nCOMMIT;\nCREATE TABLE b (id int);",
		"BEGIN; CREATE TABLE a (id int); END;",
		"/* lead */ start transaction; SELECT 1;",
		"-- note\nabort",
		"PREPARE TRANSACTION 'x'",
	} {
		dir := writeMigrations(t, map[string]string{"001_a.sql": content})
		db := &fakeDB{applied: map[string]string{}}
		_, err := Run(db, dir, Options{})
		if err == nil || !strings.Contains(err.Error(), "001_a.sql") {
			t.Errorf("%q: err = %v, want it refused", content, err)
		}
		if len(db.scripts) != 0 {
			t.Errorf("%q: the database was touched: %v", content, db.scripts)
		}
	}

	// Savepoints stay inside the transaction, and words inside bodies and
	// literals are not statements.
	for _, content := range []string{
		"SAVEPOINT s; SELECT 1; ROLLBACK TO SAVEPOINT s; RELEASE s;",
		"DO $$ BEGIN PERFORM 1; END $$;",
		"SELECT 'COMMIT';",
		"CREATE FUNCTION f() RETURNS int LANGUAGE sql\nBEGIN ATOMIC\n  SELECT 1;\n  SELECT CASE WHEN true THEN 1 END;\nEND;",
	} {
		dir := writeMigrations(t, map[string]string{"001_a.sql": content})
		db := &fakeDB{applied: map[string]string{}}
		if _, err := Run(db, dir, Options{}); err != nil {
			t.Errorf("%q: %v", content, err)
		}
	}
}

// The backstop: a file that ends the transaction in a way the check cannot
// see is not recorded.
func TestRunNoticesAnEndedTransaction(t *testing.T) {
	dir := writeMigrations(t, map[string]string{"001_a.sql": "CALL commits();"})
	db := &committingDB{fakeDB{applied: map[string]string{}}}
	_, err := Run(db, dir, Options{})
	if err == nil || !strings.Contains(err.Error(), "ended pgc's transaction") {
		t.Fatalf("err = %v", err)
	}
	if db.has("INSERT INTO public.pgc_migrations") {
		t.Fatal("the file was recorded")
	}
}

// committingDB imitates a procedure that commits: the call leaves the session
// outside a transaction.
type committingDB struct{ fakeDB }

func (c *committingDB) Exec(sql string) error {
	err := c.fakeDB.Exec(sql)
	if strings.HasPrefix(sql, "CALL") {
		c.tx = 'I'
	}
	return err
}

func TestRunOutOfOrderWarning(t *testing.T) {
	dir := writeMigrations(t, map[string]string{
		"001_old.sql": "SELECT 1;",
		"002_two.sql": "two",
		"003_new.sql": "SELECT 3;",
	})
	// 002 was applied on another branch; 001 is a merge latecomer.
	twoHash := sha256hex([]byte("two"))
	db := &fakeDB{applied: map[string]string{"002_two.sql": twoHash}}

	res, err := Run(db, dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Applied) != 2 {
		t.Fatalf("applied = %v", res.Applied)
	}
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "out-of-order") {
		t.Fatalf("warnings = %v", res.Warnings)
	}
}

func TestRunRollsBackOnFailure(t *testing.T) {
	dir := writeMigrations(t, map[string]string{
		"001_bad.sql": "CREATE TABLE broken;",
	})
	db := &fakeDB{applied: map[string]string{}, failOn: "broken"}

	res, err := Run(db, dir, Options{})
	if err == nil || !strings.Contains(err.Error(), "001_bad.sql") {
		t.Fatalf("err = %v", err)
	}
	if len(res.Applied) != 0 {
		t.Fatalf("applied = %v", res.Applied)
	}
	if !db.has("ROLLBACK") {
		t.Error("no ROLLBACK issued")
	}
	if db.has("INSERT INTO public.pgc_migrations") {
		t.Error("bookkeeping row must not be inserted for a failed file")
	}
}

func TestRunNoTransactionMarker(t *testing.T) {
	dir := writeMigrations(t, map[string]string{
		"001_conc.sql": "-- pgc: no-transaction\n" +
			"CREATE INDEX CONCURRENTLY idx_a ON a (id);\n" +
			"CREATE INDEX CONCURRENTLY idx_b ON a (id);\n",
	})
	db := &fakeDB{applied: map[string]string{}}

	if _, err := Run(db, dir, Options{}); err != nil {
		t.Fatal(err)
	}
	if db.has("BEGIN") {
		t.Error("no-transaction file must not open a transaction")
	}
	if !db.has("idx_a") || !db.has("idx_b") {
		t.Error("statements were not executed individually")
	}
	if !db.has("'started')") || !db.has("SET state = 'applied'") {
		t.Errorf("the file was not bracketed by started and applied:\n%s",
			strings.Join(db.scripts, "\n---\n"))
	}
}

// A no-transaction file that fails part way is recorded as failed, so the
// next run does not repeat its earlier statements.
func TestRunNoTransactionFailureIsRecorded(t *testing.T) {
	dir := writeMigrations(t, map[string]string{
		"001_conc.sql": "-- pgc: no-transaction\nINSERT INTO c VALUES (1);\nSELECT broken;\n",
	})
	db := &fakeDB{applied: map[string]string{}, failOn: "broken"}
	_, err := Run(db, dir, Options{})
	if err == nil || !strings.Contains(err.Error(), "statement 2") {
		t.Fatalf("err = %v", err)
	}
	if !db.has("SET state = 'failed'") {
		t.Fatal("the failure was not recorded")
	}
}

func TestStatus(t *testing.T) {
	dir := writeMigrations(t, map[string]string{
		"001_a.sql": "SELECT 1;",
		"002_b.sql": "SELECT 2;",
		"003_c.sql": "SELECT 3;",
		"004_d.sql": "SELECT 4;",
	})
	db := &fakeDB{
		applied: map[string]string{
			"001_a.sql":    sha256hex([]byte("SELECT 1;")),
			"002_b.sql":    "edited-since",
			"003_c.sql":    sha256hex([]byte("SELECT 3;")),
			"000_lost.sql": "x",
		},
		states: map[string]string{"003_c.sql": "started"},
	}

	list, err := Status(db, dir)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, m := range list {
		got[m.Name] = m.State
	}
	want := map[string]string{
		"001_a.sql": "applied", "002_b.sql": "changed", "003_c.sql": "started",
		"004_d.sql": "pending", "000_lost.sql": "missing",
	}
	for name, state := range want {
		if got[name] != state {
			t.Errorf("%s: state %q, want %q", name, got[name], state)
		}
	}
	for _, s := range db.scripts {
		if !strings.HasPrefix(s, "SELECT") {
			t.Errorf("status wrote to the database: %q", s)
		}
	}
}

// Status on a database pgc has never migrated creates nothing.
func TestStatusOnAFreshDatabase(t *testing.T) {
	dir := writeMigrations(t, map[string]string{"001_a.sql": "SELECT 1;"})
	db := &fakeDB{noTable: true}
	list, err := Status(db, dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].State != "pending" {
		t.Fatalf("list = %+v", list)
	}
	if db.has("CREATE") {
		t.Fatal("status created the history table")
	}
}

func TestSplitStatements(t *testing.T) {
	sql := `
CREATE TABLE t (name text DEFAULT 'a;b');
-- comment; with semicolon
CREATE FUNCTION f() RETURNS void AS $body$
BEGIN
  PERFORM 1;
  PERFORM 2;
END;
$body$ LANGUAGE plpgsql;
/* block; comment /* nested; */ still; */
INSERT INTO t VALUES ('it''s; fine');
SELECT "quoted;ident" FROM t
`
	stmts := splitStatements(sql)
	if len(stmts) != 4 {
		t.Fatalf("got %d statements:\n%s", len(stmts), strings.Join(stmts, "\n===\n"))
	}
	if !strings.Contains(stmts[1], "PERFORM 2;") {
		t.Errorf("dollar-quoted body was split: %q", stmts[1])
	}
	if !strings.Contains(stmts[2], "it''s; fine") {
		t.Errorf("string literal was split: %q", stmts[2])
	}
}

// An E'...' escape string with a backslash-escaped quote and an embedded
// semicolon must stay one statement.
func TestSplitStatementsEString(t *testing.T) {
	sql := `INSERT INTO t VALUES (E'a\';b'); SELECT 1`
	stmts := splitStatements(sql)
	if len(stmts) != 2 {
		t.Fatalf("got %d statements: %v", len(stmts), stmts)
	}
	if !strings.Contains(stmts[0], `E'a\';b'`) {
		t.Errorf("E-string was split: %q", stmts[0])
	}
	if stmts[1] != "SELECT 1" {
		t.Errorf("second statement = %q", stmts[1])
	}
}

// A SQL-standard function body holds semicolons of its own; it is one
// statement, as psql reads it, including CASE ... END inside it.
func TestSplitStatementsBeginAtomic(t *testing.T) {
	sql := `CREATE OR REPLACE FUNCTION f(x int) RETURNS int LANGUAGE sql
BEGIN ATOMIC
  SELECT CASE WHEN x > 0 THEN 1 ELSE 0 END;
  SELECT 2;
END;
CREATE INDEX CONCURRENTLY i ON t (a);
SELECT a$b FROM t`
	stmts := splitStatements(sql)
	if len(stmts) != 3 {
		t.Fatalf("got %d statements:\n%s", len(stmts), strings.Join(stmts, "\n===\n"))
	}
	if !strings.HasSuffix(stmts[0], "END") {
		t.Errorf("the function body was split: %q", stmts[0])
	}
	if stmts[2] != "SELECT a$b FROM t" {
		t.Errorf("an identifier with $ was misread: %q", stmts[2])
	}
}
