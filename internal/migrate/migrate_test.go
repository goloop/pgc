package migrate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goloop/pgc/internal/pgwire"
)

// fakeDB records every executed script and serves the applied-migrations
// table from a map.
type fakeDB struct {
	scripts []string
	applied map[string]string // name -> hash
	failOn  string            // substring that makes Query fail
}

func v(s string) pgwire.Value { return pgwire.Value{S: s, Valid: true} }

func (f *fakeDB) Query(sql string) ([][]pgwire.Value, error) {
	f.scripts = append(f.scripts, sql)
	if f.failOn != "" && strings.Contains(sql, f.failOn) {
		return nil, &pgwire.ServerError{Severity: "ERROR", Code: "42601", Message: "boom"}
	}
	if strings.HasPrefix(sql, "SELECT name, hash") {
		var rows [][]pgwire.Value
		for name, hash := range f.applied {
			rows = append(rows, []pgwire.Value{v(name), v(hash)})
		}
		return rows, nil
	}
	if strings.HasPrefix(sql, "SELECT name, to_char") {
		var rows [][]pgwire.Value
		for name := range f.applied {
			rows = append(rows, []pgwire.Value{v(name), v("2026-07-11 10:00")})
		}
		return rows, nil
	}
	return nil, nil
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

	res, err := Run(db, dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Applied) != 2 || res.Applied[0] != "001_a.sql" || res.Applied[1] != "002_b.sql" {
		t.Fatalf("applied = %v", res.Applied)
	}
	// Lock taken and released, table ensured, transactions used.
	for _, want := range []string{
		"pg_advisory_lock(7366499)", "pg_advisory_unlock(7366499)",
		"CREATE TABLE IF NOT EXISTS public.pgc_migrations",
		"BEGIN", "COMMIT",
		"INSERT INTO public.pgc_migrations (name, hash) VALUES ('001_a.sql'",
	} {
		if !db.has(want) {
			t.Errorf("missing %q in executed scripts:\n%s", want,
				strings.Join(db.scripts, "\n---\n"))
		}
	}
}

func TestRunSkipsAppliedAndWarnsOnDrift(t *testing.T) {
	content := "CREATE TABLE a (id int);"
	dir := writeMigrations(t, map[string]string{"001_a.sql": content})
	db := &fakeDB{applied: map[string]string{
		"001_a.sql": "not-the-real-hash",
	}}

	res, err := Run(db, dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Applied) != 0 {
		t.Fatalf("applied = %v, want none", res.Applied)
	}
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "changed after") {
		t.Fatalf("warnings = %v", res.Warnings)
	}
}

func TestRunOutOfOrderWarning(t *testing.T) {
	dir := writeMigrations(t, map[string]string{
		"001_old.sql": "SELECT 1;",
		"003_new.sql": "SELECT 3;",
	})
	// 002 was applied on another branch; 001 is a merge latecomer.
	twoHash := sha256hex([]byte("two"))
	db := &fakeDB{applied: map[string]string{"002_two.sql": twoHash}}

	res, err := Run(db, dir)
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

	res, err := Run(db, dir)
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

	if _, err := Run(db, dir); err != nil {
		t.Fatal(err)
	}
	if db.has("BEGIN") {
		t.Error("no-transaction file must not open a transaction")
	}
	if !db.has("idx_a") || !db.has("idx_b") {
		t.Error("statements were not executed individually")
	}
}

func TestStatus(t *testing.T) {
	dir := writeMigrations(t, map[string]string{
		"001_a.sql": "SELECT 1;",
		"002_b.sql": "SELECT 2;",
	})
	aHash := sha256hex([]byte("SELECT 1;"))
	db := &fakeDB{applied: map[string]string{
		"001_a.sql":    aHash,
		"000_lost.sql": "x",
	}}

	list, warnings, err := Status(db, dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || !list[0].Applied || list[1].Applied {
		t.Fatalf("list = %+v", list)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "000_lost.sql") {
		t.Fatalf("warnings = %v", warnings)
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
