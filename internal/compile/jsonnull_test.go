package compile

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"io"
	"regexp"
	"strings"
	"testing"

	"github.com/goloop/pgc/internal/pgwire"
)

// jsonDB serves a catalog with one table carrying both a NOT NULL and a
// nullable jsonb column, so the two renderings can be compared side by side.
//
//	public.widgets: id int8 NOT NULL, meta jsonb NOT NULL, state jsonb NULL
type jsonDB struct {
	statements map[string]*pgwire.Statement
}

func (j *jsonDB) Describe(query string) (*pgwire.Statement, error) {
	st, ok := j.statements[query]
	if !ok {
		return nil, &pgwire.ServerError{
			Severity: "ERROR", Code: "42601", Message: "no such fixture",
		}
	}
	return st, nil
}

func (j *jsonDB) Query(sql string) ([][]pgwire.Value, error) {
	v := func(s string) pgwire.Value { return pgwire.Value{S: s, Valid: true} }
	switch {
	case strings.Contains(sql, "pg_enum"):
		return nil, nil
	case strings.Contains(sql, "pg_type"):
		return [][]pgwire.Value{
			{v("20"), v("int8"), v("b"), v("N"), v("0"), v("0")},
			{v("3802"), v("jsonb"), v("b"), v("U"), v("0"), v("0")},
		}, nil
	case strings.Contains(sql, "pg_attribute"):
		return [][]pgwire.Value{
			{v("300"), v("1"), v("id"), v("20"), v("t")},
			{v("300"), v("2"), v("meta"), v("3802"), v("t")},
			{v("300"), v("3"), v("state"), v("3802"), v("f")},
		}, nil
	case strings.Contains(sql, "pg_class"):
		return [][]pgwire.Value{{v("300"), v("widgets"), v("public")}}, nil
	}
	return nil, nil
}

// generateWidgets renders the widgets query under the given nullable mode.
func generateWidgets(t *testing.T, nullable string) string {
	t.Helper()

	query := "SELECT id, meta, state FROM widgets WHERE id = $1"
	dir := writeQueries(t, "-- name: GetWidget :one\n"+query+";\n")

	db := &jsonDB{statements: map[string]*pgwire.Statement{
		query: {
			ParamOIDs: []uint32{20},
			Columns: []pgwire.Column{
				col(300, 1, "id", 20),
				col(300, 2, "meta", 3802),
				col(300, 3, "state", 3802),
			},
		},
	}}

	cfg := testConfig(dir)
	cfg.Nullable = nullable
	cfg.Rename = nil

	res, err := Run(db, cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range res.Files {
		if f.Name == "models.go" {
			// Field alignment is gofmt's business and shifts with the
			// longest type in the struct, so it is collapsed away before
			// the assertions look for a rendering.
			return spaces.ReplaceAllString(string(f.Data), " ")
		}
	}
	t.Fatal("models.go was not generated")
	return ""
}

// spaces collapses runs of blanks so an assertion can name a field and its
// type without depending on how wide the neighbouring column is.
var spaces = regexp.MustCompile(`[ \t]+`)

// A nullable json column must be rendered so that a NULL from the database can
// actually be scanned into it. A bare json.RawMessage cannot (see
// TestRawMessageCannotScanNull), which is why it is not inherently nullable.
func TestNullableJSONIsWrapped(t *testing.T) {
	tests := []struct {
		nullable string
		want     string
	}{
		{"pointer", "State *json.RawMessage"},
		{"sqlnull", "State sql.Null[json.RawMessage]"},
	}

	for _, tt := range tests {
		t.Run(tt.nullable, func(t *testing.T) {
			models := generateWidgets(t, tt.nullable)
			if !strings.Contains(models, tt.want) {
				t.Errorf("models.go missing %q:\n%s", tt.want, models)
			}
			// The NOT NULL column keeps the plain type in both modes: this
			// fix must not wrap what can never be NULL.
			if !strings.Contains(models, "Meta json.RawMessage") {
				t.Errorf("models.go lost the plain NOT NULL rendering:\n%s", models)
			}
		})
	}
}

// This is the defect the wrapping exists for. database/sql matches *[]byte by
// exact type; json.RawMessage is a named type, so it misses that branch and a
// NULL row fails to scan. The pointer form does scan, which is what the
// generator now emits.
func TestRawMessageCannotScanNull(t *testing.T) {
	db := sql.OpenDB(nullConnector{})
	defer db.Close()

	var bare json.RawMessage
	if err := db.QueryRow("null").Scan(&bare); err == nil {
		t.Error("a bare json.RawMessage scanned NULL; the wrapping would be " +
			"unnecessary and this test is the reason it exists")
	}

	var ptr *json.RawMessage
	if err := db.QueryRow("null").Scan(&ptr); err != nil {
		t.Errorf("*json.RawMessage failed to scan NULL: %v", err)
	}
	if ptr != nil {
		t.Errorf("*json.RawMessage = %q after NULL, want nil", *ptr)
	}

	var wrapped sql.Null[json.RawMessage]
	if err := db.QueryRow("null").Scan(&wrapped); err != nil {
		t.Errorf("sql.Null[json.RawMessage] failed to scan NULL: %v", err)
	}
	if wrapped.Valid {
		t.Error("sql.Null[json.RawMessage] reported a valid value for NULL")
	}
}

// The other half: a real value must still arrive intact through both
// renderings. NULL support is worthless if it costs the value.
func TestWrappedJSONScansValue(t *testing.T) {
	db := sql.OpenDB(nullConnector{})
	defer db.Close()

	const want = `{"a":1}`

	var ptr *json.RawMessage
	if err := db.QueryRow(want).Scan(&ptr); err != nil {
		t.Fatalf("*json.RawMessage failed to scan a value: %v", err)
	}
	if ptr == nil || string(*ptr) != want {
		t.Errorf("*json.RawMessage = %v, want %s", ptr, want)
	}

	var wrapped sql.Null[json.RawMessage]
	if err := db.QueryRow(want).Scan(&wrapped); err != nil {
		t.Fatalf("sql.Null[json.RawMessage] failed to scan a value: %v", err)
	}
	if !wrapped.Valid || string(wrapped.V) != want {
		t.Errorf("sql.Null[json.RawMessage] = %+v, want %s", wrapped, want)
	}
}

// A nil *json.RawMessage must reach the driver as a NULL parameter, so a
// round trip through the generated code preserves absence.
func TestNilPointerSendsNull(t *testing.T) {
	db := sql.OpenDB(nullConnector{})
	defer db.Close()

	var arg *json.RawMessage
	var out *json.RawMessage
	if err := db.QueryRow("param", arg).Scan(&out); err != nil {
		t.Fatalf("query with a nil parameter failed: %v", err)
	}
	if out != nil {
		t.Errorf("a nil parameter came back as %q, want NULL", *out)
	}
}

// The fake driver below answers one row whose only column is derived from the
// query text: "null" yields SQL NULL, "param" echoes the first argument, and
// anything else is returned as bytes. It exists so the scan semantics above
// are proven against database/sql itself rather than assumed.

type nullConnector struct{}

func (nullConnector) Connect(context.Context) (driver.Conn, error) {
	return nullConn{}, nil
}
func (nullConnector) Driver() driver.Driver { return nullDriver{} }

type nullDriver struct{}

func (nullDriver) Open(string) (driver.Conn, error) { return nullConn{}, nil }

type nullConn struct{}

func (nullConn) Prepare(query string) (driver.Stmt, error) { return nullStmt{q: query}, nil }
func (nullConn) Close() error                              { return nil }
func (nullConn) Begin() (driver.Tx, error)                 { return nil, io.EOF }

type nullStmt struct{ q string }

func (nullStmt) Close() error { return nil }
func (s nullStmt) NumInput() int {
	if s.q == "param" {
		return 1
	}
	return 0
}
func (nullStmt) Exec([]driver.Value) (driver.Result, error) { return nil, io.EOF }

func (s nullStmt) Query(args []driver.Value) (driver.Rows, error) {
	switch s.q {
	case "null":
		return &nullRows{value: nil}, nil
	case "param":
		return &nullRows{value: args[0]}, nil
	default:
		return &nullRows{value: []byte(s.q)}, nil
	}
}

type nullRows struct {
	value driver.Value
	done  bool
}

func (*nullRows) Columns() []string { return []string{"v"} }
func (*nullRows) Close() error      { return nil }

func (r *nullRows) Next(dest []driver.Value) error {
	if r.done {
		return io.EOF
	}
	r.done = true
	dest[0] = r.value
	return nil
}
