package queryfile

import (
	"strings"
	"testing"
)

const sample = `-- Queries for the users table.

-- name: GetUser :one
-- Returns a single user by primary key.
SELECT id, email FROM users WHERE id = $1;

-- name: CreateUser :one
-- override: $2 *string
-- param: $1 email
INSERT INTO users (email, name)
VALUES ($1, $2)
RETURNING id;

-- name: Purge :exec
DELETE FROM users
-- old rows only
WHERE created_at < now() - interval '1 day';
`

func TestParseFile(t *testing.T) {
	qs, err := ParseFile("users.sql", []byte(sample))
	if err != nil {
		t.Fatal(err)
	}
	if len(qs) != 3 {
		t.Fatalf("queries = %d", len(qs))
	}

	get := qs[0]
	if get.Name != "GetUser" || get.Command != "one" || get.Line != 3 {
		t.Errorf("GetUser = %+v", get)
	}
	if get.Doc != "Returns a single user by primary key." {
		t.Errorf("doc = %q", get.Doc)
	}
	if get.SQL != "SELECT id, email FROM users WHERE id = $1" {
		t.Errorf("sql = %q", get.SQL)
	}

	create := qs[1]
	if len(create.Overrides) != 1 ||
		create.Overrides[0].Param != 2 || create.Overrides[0].GoType != "*string" {
		t.Errorf("overrides = %+v", create.Overrides)
	}
	if create.ParamNames[1] != "email" {
		t.Errorf("param names = %v", create.ParamNames)
	}
	if !strings.HasSuffix(create.SQL, "RETURNING id") {
		t.Errorf("sql = %q", create.SQL)
	}

	// The comment inside Purge's body stays in the SQL.
	if !strings.Contains(qs[2].SQL, "-- old rows only") {
		t.Errorf("body comment lost: %q", qs[2].SQL)
	}
}

func TestParseFileErrors(t *testing.T) {
	cases := []struct{ name, src, want string }{
		{"unexported", "-- name: getUser :one\nSELECT 1;", "exported"},
		{"bad command", "-- name: X :both\nSELECT 1;", "unknown command"},
		{"iter reserved", "-- name: X :iter\nSELECT 1;", "later phase"},
		{"empty body", "-- name: X :one\n\n-- name: Y :one\nSELECT 1;", "no SQL body"},
		{"no queries", "SELECT 1;", "no \"-- name:\""},
		{"bad override", "-- name: X :one\n-- override: col\nSELECT 1;", "override"},
		{"bad param", "-- name: X :one\n-- param: x y\nSELECT 1;", "param"},
	}
	for _, c := range cases {
		if _, err := ParseFile("t.sql", []byte(c.src)); err == nil ||
			!strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want %q", c.name, err, c.want)
		}
	}
}

func TestParseOverrideForms(t *testing.T) {
	o, err := parseOverride(" total int64 notnull")
	if err != nil || o.Column != "total" || o.GoType != "int64" || o.Null != "notnull" {
		t.Errorf("o = %+v err = %v", o, err)
	}
	o, err = parseOverride(" avatar_url nullable")
	if err != nil || o.Column != "avatar_url" || o.GoType != "" || o.Null != "nullable" {
		t.Errorf("o = %+v err = %v", o, err)
	}
	o, err = parseOverride(" $3 *time.Time")
	if err != nil || o.Param != 3 || o.GoType != "*time.Time" {
		t.Errorf("o = %+v err = %v", o, err)
	}
}
