package compile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goloop/pgc/internal/config"
	"github.com/goloop/pgc/internal/pgwire"
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
			{v("20"), v("int8"), v("b")},
			{v("25"), v("text"), v("b")},
		}, nil
	case strings.Contains(sql, "pg_attribute"):
		return [][]pgwire.Value{
			{v("100"), v("1"), v("id"), v("20"), v("t")},
			{v("100"), v("2"), v("email"), v("25"), v("t")},
			{v("100"), v("3"), v("bio"), v("25"), v("f")},
		}, nil
	case strings.Contains(sql, "pg_class"):
		return [][]pgwire.Value{{v("100"), v("users")}}, nil
	}
	return nil, nil
}

func usersCol(attnum int16, name string, typ uint32) pgwire.Column {
	return pgwire.Column{Name: name, TypeOID: typ, TableOID: 100, Attnum: attnum}
}

func writeQueries(t *testing.T, src string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "users.sql"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func testConfig(dir string) config.Config {
	return config.Config{
		Queries: dir, Out: "db", Package: "db",
		Nullable: "pointer",
		Rename:   map[string]string{"users": "User"},
	}
}

func TestRunFullPipeline(t *testing.T) {
	getSQL := "SELECT id, email, bio FROM users WHERE id = $1"
	countSQL := "SELECT count(*) AS total FROM users"
	pairSQL := "SELECT id, email FROM users ORDER BY id"

	dir := writeQueries(t, `-- name: GetUser :one
-- Returns a single user by primary key.
`+getSQL+`;

-- name: CountUsers :one
-- override: total int64 notnull
`+countSQL+`;

-- name: ListPairs :many
`+pairSQL+`;
`)

	db := &fakeDB{statements: map[string]*pgwire.Statement{
		getSQL: {
			ParamOIDs: []uint32{20},
			Columns: []pgwire.Column{
				usersCol(1, "id", 20), usersCol(2, "email", 25), usersCol(3, "bio", 25),
			},
		},
		countSQL: {
			Columns: []pgwire.Column{{Name: "total", TypeOID: 20}},
		},
		pairSQL: {
			Columns: []pgwire.Column{usersCol(1, "id", 20), usersCol(2, "email", 25)},
		},
	}}

	res, err := Run(db, testConfig(dir))
	if err != nil {
		t.Fatal(err)
	}

	byName := map[string]string{}
	for _, f := range res.Files {
		byName[f.Name] = string(f.Data)
	}
	if len(byName) != 3 {
		t.Fatalf("files = %v", len(byName))
	}

	models := byName["models.go"]
	// bio is nullable -> *string under the pointer policy.
	for _, want := range []string{"type User struct {", "Bio   *string", "ID    int64"} {
		if !strings.Contains(models, want) {
			t.Errorf("models.go missing %q:\n%s", want, models)
		}
	}

	users := byName["users.sql.go"]
	for _, want := range []string{
		// Full-table match reuses the model.
		"func (q *Queries) GetUser(ctx context.Context, id int64) (User, error) {",
		"row.Scan(&u.ID, &u.Email, &u.Bio)",
		// Scalar with an override forcing NOT NULL.
		"func (q *Queries) CountUsers(ctx context.Context) (int64, error) {",
		// Projection -> row struct.
		"type ListPairsRow struct {",
		"func (q *Queries) ListPairs(ctx context.Context) ([]ListPairsRow, error) {",
		// Synthesized and annotated docs.
		"// GetUser returns a single user by primary key.",
		"// ListPairs runs the query and returns the matching rows.",
	} {
		if !strings.Contains(users, want) {
			t.Errorf("users.sql.go missing %q:\n%s", want, users)
		}
	}
}

func TestRunOuterJoinWarning(t *testing.T) {
	sql := "SELECT u.id, u.email FROM users u LEFT JOIN users x ON true"
	dir := writeQueries(t, "-- name: Joined :many\n"+sql+";\n")

	db := &fakeDB{statements: map[string]*pgwire.Statement{
		sql: {Columns: []pgwire.Column{
			usersCol(1, "id", 20), usersCol(2, "email", 25),
		}},
	}}
	res, err := Run(db, testConfig(dir))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "outer join") {
		t.Errorf("warnings = %v", res.Warnings)
	}
}

func TestRunErrors(t *testing.T) {
	cases := []struct {
		name, src string
		st        *pgwire.Statement
		want      string
	}{
		{
			"one without rows",
			"-- name: Nothing :one\nDELETE FROM users;\n",
			&pgwire.Statement{},
			"returns no rows",
		},
		{
			"unnamed expression",
			"-- name: Bad :many\nSELECT id, 1 FROM users;\n",
			&pgwire.Statement{Columns: []pgwire.Column{
				usersCol(1, "id", 20), {Name: "?column?", TypeOID: 20},
			}},
			"AS",
		},
		{
			"override typo",
			"-- name: Fine :one\n-- override: nope int64\nSELECT id, email FROM users;\n",
			&pgwire.Statement{Columns: []pgwire.Column{
				usersCol(1, "id", 20), usersCol(2, "email", 25),
			}},
			"does not return",
		},
	}
	for _, c := range cases {
		dir := writeQueries(t, c.src)
		sql := strings.TrimSuffix(strings.SplitN(c.src, "\n", 4)[len(strings.SplitN(c.src, "\n", 4))-1], ";\n")
		_ = sql
		db := &fakeDB{statements: map[string]*pgwire.Statement{}}
		// Register under whatever body the file parses to.
		qs, err := queryfileParse(dir)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		db.statements[qs[0].SQL] = c.st

		if _, err := Run(db, testConfig(dir)); err == nil ||
			!strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want %q", c.name, err, c.want)
		}
	}
}

func TestRunDuplicateNames(t *testing.T) {
	dir := writeQueries(t, `-- name: Same :one
SELECT id, email FROM users;

-- name: Same :one
SELECT id, email FROM users;
`)
	db := &fakeDB{statements: map[string]*pgwire.Statement{}}
	if _, err := Run(db, testConfig(dir)); err == nil ||
		!strings.Contains(err.Error(), "already defined") {
		t.Errorf("err = %v", err)
	}
}
