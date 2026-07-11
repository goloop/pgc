package gen

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite the golden files")

// usersInput is the fixture from the approved design (PGC_PLAN section 5):
// a users table and the four canonical queries. The golden files ARE the
// specification of the generated code's shape - review any diff carefully.
func usersInput() Input {
	userFields := []Field{
		{Name: "id", Type: "int64"},
		{Name: "email", Type: "string"},
		{Name: "name", Type: "string"},
		{Name: "created_at", Type: "time.Time"},
	}
	return Input{
		Package: "db",
		Models: []Model{
			{Name: "User", Table: "users", Fields: userFields},
		},
		Files: []SrcFile{{
			Source: "queries/users.sql",
			Out:    "users.sql.go",
			Queries: []Query{
				{
					Name: "GetUser",
					Doc: "GetUser returns a single user by primary key. " +
						"It returns sql.ErrNoRows when no user matches.",
					Command: "one",
					SQL:     "SELECT id, email, name, created_at\nFROM users\nWHERE id = $1",
					Params:  []Param{{Name: "id", Type: "int64"}},
					Ret:     Ret{Kind: RetModel, Type: "User", Fields: userFields},
				},
				{
					Name:    "ListUsers",
					Doc:     "ListUsers returns users ordered from newest to oldest.",
					Command: "many",
					SQL: "SELECT id, email, name, created_at\nFROM users\n" +
						"ORDER BY created_at DESC\nLIMIT $1 OFFSET $2",
					Params: []Param{
						// The live server types LIMIT and OFFSET as bigint.
						{Name: "limit", Type: "int64"},
						{Name: "offset", Type: "int64"},
					},
					Ret: Ret{Kind: RetModel, Type: "User", Fields: userFields},
				},
				{
					Name:    "CreateUser",
					Doc:     "CreateUser inserts a user and returns the stored row.",
					Command: "one",
					SQL: "INSERT INTO users (email, name)\nVALUES ($1, $2)\n" +
						"RETURNING id, email, name, created_at",
					Params: []Param{
						{Name: "email", Type: "string"},
						{Name: "name", Type: "string"},
					},
					Ret: Ret{Kind: RetModel, Type: "User", Fields: userFields},
				},
				{
					Name:    "DeleteUser",
					Doc:     "DeleteUser removes a user by primary key.",
					Command: "exec",
					SQL:     "DELETE FROM users\nWHERE id = $1",
					Params:  []Param{{Name: "id", Type: "int64"}},
					Ret:     Ret{Kind: RetNone},
				},
			},
		}},
	}
}

func TestGolden(t *testing.T) {
	files, err := Render(usersInput())
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 3 {
		t.Fatalf("files = %d, want db.go, models.go, users.sql.go", len(files))
	}

	for _, f := range files {
		golden := filepath.Join("testdata", "golden", f.Name+".golden")
		if *update {
			os.MkdirAll(filepath.Dir(golden), 0o755)
			if err := os.WriteFile(golden, f.Data, 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		want, err := os.ReadFile(golden)
		if err != nil {
			t.Fatalf("%s: %v (run go test -update to create)", f.Name, err)
		}
		if string(f.Data) != string(want) {
			t.Errorf("%s does not match its golden file.\n--- got ---\n%s",
				f.Name, f.Data)
		}
	}
}

// TestRowStructAndParamsStruct locks the two shapes the fixture above does
// not cover: a projection Row struct and a >=4 params struct.
func TestRowStructAndParamsStruct(t *testing.T) {
	in := Input{
		Package: "db",
		Files: []SrcFile{{
			Source: "queries/report.sql",
			Out:    "report.sql.go",
			Queries: []Query{{
				Name:    "SearchOrders",
				Doc:     "SearchOrders runs the query and returns the matching rows.",
				Command: "many",
				SQL:     "SELECT o.id, o.total, u.email FROM orders o JOIN users u ON true",
				Params: []Param{
					{Name: "status", Type: "string"},
					{Name: "since", Type: "time.Time"},
					{Name: "limit", Type: "int32"},
					{Name: "offset", Type: "int32"},
				},
				Ret: Ret{Kind: RetRow, Type: "SearchOrdersRow", Fields: []Field{
					{Name: "id", Type: "int64"},
					{Name: "total", Type: "string"},
					{Name: "email", Type: "string"},
				}},
			}},
		}},
	}
	files, err := Render(in)
	if err != nil {
		t.Fatal(err)
	}
	src := string(files[len(files)-1].Data)

	for _, want := range []string{
		"type SearchOrdersParams struct {",
		"\tStatus string\n\tSince  time.Time\n\tLimit  int32\n\tOffset int32\n}",
		"type SearchOrdersRow struct {",
		"func (q *Queries) SearchOrders(ctx context.Context, arg SearchOrdersParams) ([]SearchOrdersRow, error) {",
		"q.db.QueryContext(ctx, searchOrders, arg.Status, arg.Since, arg.Limit, arg.Offset)",
		"var r SearchOrdersRow",
		"rows.Scan(&r.ID, &r.Total, &r.Email)",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("missing %q in:\n%s", want, src)
		}
	}
}

func TestSnakeCase(t *testing.T) {
	cases := map[string]string{
		"Author":        "author",
		"Post":          "post",
		"RefreshRecord": "refresh_record",
		"User":          "user",
		"HTTPServer":    "http_server",
	}
	for in, want := range cases {
		if got := snakeCase(in); got != want {
			t.Errorf("snakeCase(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestEmbedJSONTagFollowsField checks that an embedded struct's json tag comes
// from the Go field (its `as` alias), not the source table name.
func TestEmbedJSONTagFollowsField(t *testing.T) {
	in := Input{
		Package:  "db",
		JSONTags: true,
		Models: []Model{
			{Name: "User", Table: "users", Fields: []Field{
				{Name: "id", Type: "int64"},
				{Name: "email", Type: "string"},
			}},
		},
		Files: []SrcFile{{
			Source: "queries/orders.sql",
			Out:    "orders.sql.go",
			Queries: []Query{{
				Name:    "OrderWithBuyer",
				Doc:     "OrderWithBuyer runs the query.",
				Command: "one",
				SQL:     "SELECT o.id, u.id, u.email FROM orders o JOIN users u ON true",
				Ret: Ret{Kind: RetRow, Type: "OrderWithBuyerRow", Fields: []Field{
					{Name: "id", Type: "int64"},
					{Name: "users", GoName: "Buyer", Type: "User", Embed: []Field{
						{Name: "id", Type: "int64"},
						{Name: "email", Type: "string"},
					}},
				}},
			}},
		}},
	}
	files, err := Render(in)
	if err != nil {
		t.Fatal(err)
	}
	src := string(files[len(files)-1].Data)
	if !strings.Contains(src, "`json:\"buyer\"`") {
		t.Errorf("embed tag should be json:\"buyer\", got:\n%s", src)
	}
	if strings.Contains(src, "`json:\"users\"`") {
		t.Errorf("embed tag must not be the table name json:\"users\":\n%s", src)
	}
}
