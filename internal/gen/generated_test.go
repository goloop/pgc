package gen

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// shadowInput is a package whose parameters are named after everything a
// generated body refers to: the query's own SQL constant, a package the result
// type is spelled with, a predeclared type and the array adapter.
func shadowInput() Input {
	return Input{
		Package:      "shadow",
		ArrayHelpers: []string{"stringArray"},
		Files: []SrcFile{{
			Source: "queries/shadow.sql",
			Out:    "shadow.sql.go",
			Queries: []Query{
				{
					Name:    "Put",
					Doc:     "Put stores one value.",
					Command: "exec",
					SQL:     "INSERT INTO texts(value) VALUES ($1)",
					Params:  []Param{{Name: "put", Type: "string"}},
				},
				{
					Name:    "Stamp",
					Doc:     "Stamp reads a timestamp.",
					Command: "one",
					SQL:     "SELECT $1::timestamptz + $2::interval",
					Params: []Param{
						{Name: "time", Type: "time.Time"},
						{Name: "string", Type: "string"},
					},
					Ret: Ret{Kind: RetScalar, Type: "time.Time"},
				},
				{
					Name:    "Tags",
					Doc:     "Tags stores a list.",
					Command: "exec",
					SQL:     "INSERT INTO tags(list) VALUES ($1)",
					Params: []Param{
						{Name: "string_array", Type: "[]string", Helper: "stringArray"},
					},
				},
			},
		}},
	}
}

// shadowProbe drives the generated package through a recording DBTX. It is
// the behavioural half of the check: code where a parameter shadows the SQL
// constant compiles, so only running it shows which string reaches the driver.
const shadowProbe = `package shadow

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"testing"
)

type recorder struct {
	sql  string
	args []any
}

func (r *recorder) ExecContext(_ context.Context, q string, args ...any) (sql.Result, error) {
	r.sql, r.args = q, args
	return driver.RowsAffected(1), nil
}

func (r *recorder) QueryContext(context.Context, string, ...any) (*sql.Rows, error) {
	return nil, errors.New("unused")
}

func (r *recorder) QueryRowContext(context.Context, string, ...any) *sql.Row {
	return nil
}

func TestSQLNeverComesFromAnArgument(t *testing.T) {
	rec := &recorder{}
	input := "SELECT $1::text /* caller-controlled */"
	if err := New(rec).Put(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	if rec.sql != put {
		t.Fatalf("driver got SQL %q, want the query constant", rec.sql)
	}
	if len(rec.args) != 1 || rec.args[0] != input {
		t.Fatalf("args = %v", rec.args)
	}
}

func TestStringArrayKeepsEveryByte(t *testing.T) {
	in := stringArray{"\nvalue\n", "\rv\r", "\vv\v", "\fv\f", " v ", "", "NULL",
		"null", "a,b", "{x}", "q\"q", "b\\b", "юнікод"}
	enc, err := in.Value()
	if err != nil {
		t.Fatal(err)
	}
	var out stringArray
	if err := out.Scan(enc); err != nil {
		t.Fatal(err)
	}
	if len(out) != len(in) {
		t.Fatalf("round trip %q -> %q", in, out)
	}
	for i := range in {
		if in[i] != out[i] {
			t.Errorf("element %d: %q -> %q (encoded %q)", i, in[i], out[i], enc)
		}
	}
	if _, err := (stringArray{"a\x00b"}).Value(); err == nil {
		t.Error("a NUL byte must be refused, not silently stored")
	}
}
`

// TestGeneratedCodeKeepsSQLAndDataApart compiles and runs a generated package
// whose parameter names collide with the identifiers its bodies use. The type
// checker cannot catch a parameter that shadows the SQL constant - that code
// compiles - so the package is exercised through a recording driver.
func TestGeneratedCodeKeepsSQLAndDataApart(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a generated package with the go tool")
	}
	goTool, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go tool not on PATH")
	}

	files, err := Render(shadowInput())
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	write := func(name, data string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range files {
		write(f.Name, string(f.Data))
	}
	write("go.mod", "module example.com/shadow\n\ngo 1.24\n")
	write("probe_test.go", shadowProbe)

	cmd := exec.Command(goTool, "test", "./...")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=-mod=mod")
	if out, err := cmd.CombinedOutput(); err != nil {
		var src strings.Builder
		for _, f := range files {
			src.WriteString("// " + f.Name + "\n" + string(f.Data))
		}
		t.Fatalf("generated package failed: %v\n%s\n%s", err, out, src.String())
	}
}
