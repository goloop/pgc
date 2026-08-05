package snapshot_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goloop/pgc/internal/compile"
	"github.com/goloop/pgc/internal/config"
	"github.com/goloop/pgc/internal/pgwire"
	"github.com/goloop/pgc/internal/snapshot"
)

// fakeDB answers Describe from a canned map and serves a small catalog:
// users(id int8 NOT NULL, email text NOT NULL, bio text NULL).
type fakeDB struct {
	statements map[string]*pgwire.Statement
}

func (f *fakeDB) Describe(query string) (*pgwire.Statement, error) {
	st, ok := f.statements[query]
	if !ok {
		return nil, &pgwire.ServerError{
			Severity: "ERROR", Code: "42601", Message: "no such fixture",
		}
	}
	return st, nil
}

func (f *fakeDB) Query(sql string) ([][]pgwire.Value, error) {
	v := func(s string) pgwire.Value { return pgwire.Value{S: s, Valid: true} }
	switch {
	case strings.Contains(sql, "pg_type"):
		return [][]pgwire.Value{
			{v("20"), v("int8"), v("b"), v("N"), v("0"), v("0")},
			{v("25"), v("text"), v("b"), v("S"), v("0"), v("0")},
		}, nil
	case strings.Contains(sql, "pg_attribute"):
		return [][]pgwire.Value{
			{v("100"), v("1"), v("id"), v("20"), v("t")},
			{v("100"), v("2"), v("email"), v("25"), v("t")},
			{v("100"), v("3"), v("bio"), v("25"), v("f")},
		}, nil
	case strings.Contains(sql, "pg_class"):
		return [][]pgwire.Value{{v("100"), v("users"), v("public")}}, nil
	}
	return nil, nil
}

const (
	getSQL   = "SELECT id, email, bio FROM users WHERE id = $1"
	countSQL = "SELECT count(*) AS total FROM users"
)

func fixture(t *testing.T) (config.Config, *fakeDB) {
	t.Helper()

	dir := t.TempDir()
	src := `-- name: GetUser :one
-- Returns a single user by primary key.
` + getSQL + `;

-- name: CountUsers :one
` + countSQL + `;
`
	if err := os.WriteFile(filepath.Join(dir, "users.sql"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := config.Config{
		Queries: dir, Out: "db", Package: "db",
		Nullable: "pointer",
		Rename:   map[string]string{"users": "User"},
	}
	db := &fakeDB{statements: map[string]*pgwire.Statement{
		getSQL: {
			ParamOIDs: []uint32{20},
			Columns: []pgwire.Column{
				{Name: "id", TypeOID: 20, TableOID: 100, Attnum: 1},
				{Name: "email", TypeOID: 25, TableOID: 100, Attnum: 2},
				{Name: "bio", TypeOID: 25, TableOID: 100, Attnum: 3},
			},
		},
		countSQL: {Columns: []pgwire.Column{{Name: "total", TypeOID: 20}}},
	}}
	return cfg, db
}

// record runs the live half and returns the snapshot it would write.
func record(t *testing.T, cfg config.Config, db *fakeDB) (*snapshot.File, []compile.Described) {
	t.Helper()
	queries, err := compile.Queries(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ds, cat, err := compile.Describe(db, queries)
	if err != nil {
		t.Fatal(err)
	}
	f, err := snapshot.Of("test", cfg.Migrations, ds, cat)
	if err != nil {
		t.Fatal(err)
	}
	return f, ds
}

// TestRoundTripProducesIdenticalCode is the claim the whole feature rests on:
// generating from the record and generating from the database give the same
// package, byte for byte. Anything less and the committed file is a liability.
func TestRoundTripProducesIdenticalCode(t *testing.T) {
	cfg, db := fixture(t)

	live, err := compile.Run(db, cfg)
	if err != nil {
		t.Fatal(err)
	}

	f, _ := record(t, cfg, db)
	path := filepath.Join(t.TempDir(), snapshot.Name)
	if err := snapshot.Save(path, f); err != nil {
		t.Fatal(err)
	}
	loaded, err := snapshot.Load(path)
	if err != nil {
		t.Fatal(err)
	}

	queries, err := compile.Queries(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ds, cat, err := loaded.Describe(queries)
	if err != nil {
		t.Fatal(err)
	}
	offline, err := compile.Build(cfg, ds, cat)
	if err != nil {
		t.Fatal(err)
	}

	if len(offline.Files) != len(live.Files) {
		t.Fatalf("offline produced %d files, live produced %d",
			len(offline.Files), len(live.Files))
	}
	for i := range live.Files {
		if offline.Files[i].Name != live.Files[i].Name {
			t.Fatalf("file %d: %q offline, %q live",
				i, offline.Files[i].Name, live.Files[i].Name)
		}
		if string(offline.Files[i].Data) != string(live.Files[i].Data) {
			t.Errorf("%s differs offline:\n--- offline ---\n%s\n--- live ---\n%s",
				live.Files[i].Name, offline.Files[i].Data, live.Files[i].Data)
		}
	}
}

// TestSaveIsStable checks two runs against an unchanged schema leave the file
// alone, so it does not churn in every commit.
func TestSaveIsStable(t *testing.T) {
	cfg, db := fixture(t)

	first, _ := record(t, cfg, db)
	second, _ := record(t, cfg, db)

	dir := t.TempDir()
	a := filepath.Join(dir, "a.json")
	b := filepath.Join(dir, "b.json")
	if err := snapshot.Save(a, first); err != nil {
		t.Fatal(err)
	}
	if err := snapshot.Save(b, second); err != nil {
		t.Fatal(err)
	}

	da, _ := os.ReadFile(a)
	dbb, _ := os.ReadFile(b)
	if string(da) != string(dbb) {
		t.Errorf("two identical runs wrote different files:\n%s\n---\n%s", da, dbb)
	}
}

// TestStaleQuery covers the case that must never generate: a query whose SQL
// has moved on from what the server was asked about.
func TestStaleQuery(t *testing.T) {
	cfg, db := fixture(t)
	f, _ := record(t, cfg, db)

	// Edit the query on disk without re-describing it.
	edited := `-- name: GetUser :one
SELECT id, email FROM users WHERE id = $1;

-- name: CountUsers :one
` + countSQL + `;
`
	path := filepath.Join(cfg.Queries, "users.sql")
	if err := os.WriteFile(path, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}

	queries, err := compile.Queries(cfg)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = f.Describe(queries)
	if err == nil {
		t.Fatal("an edited query generated from a stale snapshot")
	}
	for _, want := range []string{"GetUser", "SQL has changed", snapshot.Name} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q: %v", want, err)
		}
	}
}

// TestUnknownQuery covers a query added since the snapshot: its types were
// never read, so there is nothing to generate it from.
func TestUnknownQuery(t *testing.T) {
	cfg, db := fixture(t)
	f, _ := record(t, cfg, db)

	src, err := os.ReadFile(filepath.Join(cfg.Queries, "users.sql"))
	if err != nil {
		t.Fatal(err)
	}
	added := string(src) + "\n-- name: ListUsers :many\nSELECT id FROM users;\n"
	if err := os.WriteFile(filepath.Join(cfg.Queries, "users.sql"),
		[]byte(added), 0o644); err != nil {
		t.Fatal(err)
	}

	queries, err := compile.Queries(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.Describe(queries); err == nil {
		t.Fatal("a query the snapshot has never seen was generated anyway")
	} else if !strings.Contains(err.Error(), "ListUsers") {
		t.Errorf("error does not name the new query: %v", err)
	}
}

// TestCompare is what a CI job reads: an unchanged pair says nothing, and each
// kind of drift says which query moved.
func TestCompare(t *testing.T) {
	cfg, db := fixture(t)
	recorded, _ := record(t, cfg, db)

	if diffs := recorded.Compare(recorded); len(diffs) != 0 {
		t.Errorf("a snapshot differs from itself: %v", diffs)
	}

	// The schema moved: bio is gone from the result.
	moved := *db
	moved.statements = map[string]*pgwire.Statement{
		getSQL: {
			ParamOIDs: []uint32{20},
			Columns: []pgwire.Column{
				{Name: "id", TypeOID: 20, TableOID: 100, Attnum: 1},
				{Name: "email", TypeOID: 25, TableOID: 100, Attnum: 2},
			},
		},
		countSQL: {Columns: []pgwire.Column{{Name: "total", TypeOID: 20}}},
	}
	fresh, _ := record(t, cfg, &moved)

	diffs := recorded.Compare(fresh)
	if len(diffs) != 1 || !strings.Contains(diffs[0], "GetUser") {
		t.Errorf("diffs = %v, want one naming GetUser", diffs)
	}
}

// TestMigrationsFingerprint checks the snapshot notices migrations written
// after it was taken - the one kind of staleness the queries cannot reveal.
func TestMigrationsFingerprint(t *testing.T) {
	cfg, db := fixture(t)
	cfg.Migrations = t.TempDir()

	first := filepath.Join(cfg.Migrations, "001_init.sql")
	if err := os.WriteFile(first, []byte("CREATE TABLE users ();"), 0o644); err != nil {
		t.Fatal(err)
	}

	f, _ := record(t, cfg, db)
	if f.Migrations == "" {
		t.Fatal("the migrations were not fingerprinted")
	}
	if w := f.Warnings(cfg.Migrations); len(w) != 0 {
		t.Errorf("an unchanged directory warned: %v", w)
	}

	second := filepath.Join(cfg.Migrations, "002_add_bio.sql")
	if err := os.WriteFile(second, []byte("ALTER TABLE users ADD bio text;"), 0o644); err != nil {
		t.Fatal(err)
	}
	w := f.Warnings(cfg.Migrations)
	if len(w) != 1 || !strings.Contains(w[0], cfg.Migrations) {
		t.Errorf("warnings = %v, want one naming the directory", w)
	}

	// Editing a file counts as much as adding one.
	if err := os.WriteFile(second, []byte("ALTER TABLE users ADD note text;"), 0o644); err != nil {
		t.Fatal(err)
	}
	after, _ := record(t, cfg, db)
	if after.Migrations == f.Migrations {
		t.Error("editing a migration did not change the fingerprint")
	}
}

// TestNoMigrationsDirectory checks a project without one is not an error.
func TestNoMigrationsDirectory(t *testing.T) {
	cfg, db := fixture(t)
	cfg.Migrations = filepath.Join(t.TempDir(), "absent")

	f, _ := record(t, cfg, db)
	if f.Migrations != "" {
		t.Errorf("Migrations = %q, want empty", f.Migrations)
	}
	if w := f.Warnings(cfg.Migrations); len(w) != 0 {
		t.Errorf("warnings = %v, want none", w)
	}
}

// TestLoadRejectsAnotherFormat checks a file from a different pgc is refused
// rather than half understood.
func TestLoadRejectsAnotherFormat(t *testing.T) {
	path := filepath.Join(t.TempDir(), snapshot.Name)
	body := `{"version": 99, "pgc": "9.9.9", "queries": [], "catalog": {}}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := snapshot.Load(path); err == nil {
		t.Fatal("a snapshot in an unknown format was accepted")
	}
}

// TestPath keeps the snapshot beside the configuration it belongs to.
func TestPath(t *testing.T) {
	if got := snapshot.Path(filepath.Join("a", "b", "pgc.json")); got !=
		filepath.Join("a", "b", snapshot.Name) {
		t.Errorf("Path = %q", got)
	}
}

// enumDB serves a schema with one enum, so a label can be added without any
// query describing differently.
type enumDB struct {
	fakeDB
	labels []string
}

func (e *enumDB) Query(sql string) ([][]pgwire.Value, error) {
	v := func(s string) pgwire.Value { return pgwire.Value{S: s, Valid: true} }
	if strings.Contains(sql, "pg_enum") {
		var rows [][]pgwire.Value
		for _, l := range e.labels {
			rows = append(rows, []pgwire.Value{v("300"), v(l)})
		}
		return rows, nil
	}
	if strings.Contains(sql, "pg_type") {
		return [][]pgwire.Value{
			{v("20"), v("int8"), v("b"), v("N"), v("0"), v("0")},
			{v("25"), v("text"), v("b"), v("S"), v("0"), v("0")},
			{v("300"), v("note_status"), v("e"), v("E"), v("0"), v("0")},
		}, nil
	}
	return e.fakeDB.Query(sql)
}

// TestCompareSeesTheCatalog covers the drift no query reveals: adding an enum
// label changes the generated constants, the Values list and the parser, while
// every statement still has exactly the same parameters and columns. Comparing
// only the queries would call the snapshot current.
func TestCompareSeesTheCatalog(t *testing.T) {
	dir := t.TempDir()
	const sql = "SELECT id, status FROM notes WHERE id = $1"
	src := "-- name: GetNote :one\n" + sql + ";\n"
	if err := os.WriteFile(filepath.Join(dir, "notes.sql"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := config.Config{Queries: dir, Out: "db", Package: "db", Nullable: "pointer"}
	statements := map[string]*pgwire.Statement{
		sql: {
			ParamOIDs: []uint32{20},
			Columns: []pgwire.Column{
				{Name: "id", TypeOID: 20, TableOID: 100, Attnum: 1},
				{Name: "status", TypeOID: 300},
			},
		},
	}

	before := &enumDB{fakeDB{statements}, []string{"draft", "live"}}
	after := &enumDB{fakeDB{statements}, []string{"draft", "live", "archived"}}

	describe := func(db compileDB) *snapshot.File {
		t.Helper()
		queries, err := compile.Queries(cfg)
		if err != nil {
			t.Fatal(err)
		}
		ds, cat, err := compile.Describe(db, queries)
		if err != nil {
			t.Fatal(err)
		}
		f, err := snapshot.Of("test", cfg.Migrations, ds, cat)
		if err != nil {
			t.Fatal(err)
		}
		return f
	}

	was := describe(before)
	now := describe(after)

	// The queries themselves are untouched.
	if len(was.Queries) != 1 || len(now.Queries) != 1 {
		t.Fatalf("expected one query each, got %d and %d",
			len(was.Queries), len(now.Queries))
	}
	if was.Queries[0].SQL != now.Queries[0].SQL ||
		!sameShape(was.Queries[0], now.Queries[0]) {
		t.Fatal("the fixture is wrong: the query itself changed")
	}

	diffs := was.Compare(now)
	if len(diffs) == 0 {
		t.Fatal("an added enum label went unreported")
	}
	var mentions bool
	for _, d := range diffs {
		if strings.Contains(d, "note_status") {
			mentions = true
		}
	}
	if !mentions {
		t.Errorf("diffs = %v, want one naming the enum", diffs)
	}

	if diffs := was.Compare(was); len(diffs) != 0 {
		t.Errorf("a snapshot differs from itself: %v", diffs)
	}
}

// compileDB is the slice of the database that describing needs.
type compileDB interface {
	Describe(query string) (*pgwire.Statement, error)
	Query(sql string) ([][]pgwire.Value, error)
}

// sameShape reports whether two records describe the same parameters and
// columns, so a test can prove the query half did not move.
func sameShape(a, b snapshot.Query) bool {
	if len(a.Params) != len(b.Params) || len(a.Columns) != len(b.Columns) {
		return false
	}
	for i := range a.Columns {
		if a.Columns[i] != b.Columns[i] {
			return false
		}
	}
	return true
}

// TestLoadRejectsDuplicateQueries checks a record that contradicts itself is
// refused rather than resolved by whichever entry happens to be last.
func TestLoadRejectsDuplicateQueries(t *testing.T) {
	path := filepath.Join(t.TempDir(), snapshot.Name)
	body := `{"version":1,"pgc":"test","catalog":{},"queries":[
		{"file":"queries/a.sql","name":"GetUser","sql":"sha256:aa"},
		{"file":"queries/a.sql","name":"GetUser","sql":"sha256:bb"}]}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := snapshot.Load(path); err == nil {
		t.Fatal("a snapshot recording the same query twice was accepted")
	} else if !strings.Contains(err.Error(), "GetUser") {
		t.Errorf("error does not name the query: %v", err)
	}
}
