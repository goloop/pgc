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

// TestUsesParamStruct pins the threshold itself. Everything that emits,
// signs or reserves the params struct reads this one predicate, so the
// boundary is worth stating outright.
func TestUsesParamStruct(t *testing.T) {
	params := func(n int) []Param {
		var out []Param
		for i := 0; i < n; i++ {
			out = append(out, Param{Name: "p", Type: "int64"})
		}
		return out
	}
	for n, want := range map[int]bool{0: false, 1: false, 3: false, 4: true, 9: true} {
		q := Query{Name: "Q", Params: params(n)}
		if got := q.UsesParamStruct(); got != want {
			t.Errorf("%d params: UsesParamStruct() = %v, want %v", n, got, want)
		}
	}
}

// TestQuerierImportsOnlyWhatItSpells checks that querier.go imports nothing
// on account of parameters that the interface never spells out. Past four
// parameters the signature is an XxxParams struct, so a json.RawMessage or a
// time.Time parameter must not pull encoding/json or time into a file that
// mentions neither - the generated package would not compile.
func TestQuerierImportsOnlyWhatItSpells(t *testing.T) {
	in := Input{
		Package:       "db",
		EmitInterface: true,
		Files: []SrcFile{{
			Source: "queries/docs.sql",
			Out:    "docs.sql.go",
			Queries: []Query{{
				Name:    "UpsertDoc",
				Doc:     "UpsertDoc stores a document.",
				Command: "exec",
				SQL:     "INSERT INTO docs (id, body, at, tag) VALUES ($1, $2, $3, $4)",
				Params: []Param{
					{Name: "id", Type: "int64"},
					{Name: "body", Type: "json.RawMessage"},
					{Name: "at", Type: "time.Time"},
					{Name: "tag", Type: "string"},
				},
				Ret: Ret{Kind: RetNone},
			}},
		}},
	}
	files, err := Render(in)
	if err != nil {
		t.Fatal(err)
	}

	var querier string
	for _, f := range files {
		if f.Name == "querier.go" {
			querier = string(f.Data)
		}
	}
	if querier == "" {
		t.Fatal("no querier.go rendered")
	}
	if !strings.Contains(querier,
		"UpsertDoc(ctx context.Context, arg UpsertDocParams) error") {
		t.Errorf("unexpected signature in:\n%s", querier)
	}
	for _, unwanted := range []string{"encoding/json", "\"time\""} {
		if strings.Contains(querier, unwanted) {
			t.Errorf("querier.go imports %s but never spells it out:\n%s",
				unwanted, querier)
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

func TestParamStructPreservesCollidingArguments(t *testing.T) {
	files, err := Render(Input{Package: "db", Files: []SrcFile{{Source: "joins.sql", Out: "joins.sql.go", Queries: []Query{{
		Name: "Attach", Command: "exec", SQL: "SELECT $1, $2, $3, $4",
		Params: []Param{{Name: "id", Type: "int64"}, {Name: "owner_id", Type: "int64"}, {Name: "id", Type: "int64"}, {Name: "owner_id", Type: "int64"}},
		Ret:    Ret{Kind: RetNone},
	}}}}})
	if err != nil {
		t.Fatal(err)
	}
	src := string(files[len(files)-1].Data)
	for _, want := range []string{"ID      int64", "ID_     int64", "OwnerID int64", "OwnerID_ int64", "arg.ID, arg.OwnerID, arg.ID_, arg.OwnerID_"} {
		if !strings.Contains(strings.Join(strings.Fields(src), " "), strings.Join(strings.Fields(want), " ")) {
			t.Errorf("missing %q in:\n%s", want, src)
		}
	}
}

// A local the method body declares may not shadow the query's own SQL
// constant (an :iter query named Zero declares zero before it runs the query),
// take a predeclared identifier (an accumulator named append) or, in an :iter
// body, the yield callback.
func TestLocalsNeverShadowTheQueryOrBuiltins(t *testing.T) {
	in := Input{Package: "db", Namer: NewNamer(), Files: []SrcFile{{
		Source: "q.sql", Out: "q.sql.go", Queries: []Query{
			{Name: "Zero", Command: "iter", SQL: "SELECT x FROM t",
				Ret: Ret{Kind: RetScalar, Type: "string"}},
			{Name: "ListAppend", Command: "many", SQL: "SELECT x FROM t",
				Ret: Ret{Kind: RetScalar, Type: "string"}},
			{Name: "Walk", Command: "iter", SQL: "SELECT x FROM t WHERE y = $1",
				Params: []Param{{Name: "yield", Type: "int64"}},
				Ret:    Ret{Kind: RetScalar, Type: "string"}},
			{Name: "Yield", Command: "iter", SQL: "SELECT x FROM t",
				Ret: Ret{Kind: RetScalar, Type: "string"}},
			{Name: "ListTime", Command: "many", SQL: "SELECT now() FROM t",
				Ret: Ret{Kind: RetScalar, Type: "*time.Time"}},
		},
	}}}
	out, err := Render(in)
	if err != nil {
		t.Fatal(err)
	}
	var src string
	for _, f := range out {
		if f.Name == "q.sql.go" {
			src = string(f.Data)
		}
	}
	for _, want := range []string{
		"var zero_ string",
		"q.db.QueryContext(ctx, zero)",
		"var items []string",
		"items = append(items, v)",
		"Walk(ctx context.Context, yield_ int64)",
		"q.db.QueryContext(ctx, walk, yield_)",
		// Yield's SQL constant is yield; the callback steps aside.
		"return func(yield_ func(string, error) bool) {",
		"q.db.QueryContext(ctx, yield)",
		// ListTime's accumulator may not shadow the time package.
		"var time_ []*time.Time",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("missing %q in:\n%s", want, src)
		}
	}
	// Zero's own body must not declare a plain zero before the query call;
	// Walk, whose constant is walk, may.
	zero := src[strings.Index(src, "func (q *Queries) Zero"):strings.Index(src, "const listAppend")]
	if strings.Contains(zero, "var zero string") || strings.Contains(src, "var append ") {
		t.Errorf("a local shadows the constant or a builtin in:\n%s", src)
	}
}

// An import registered for a custom type is still needed once the type has
// been wrapped for nullability or into a slice.
func TestImportsFollowWrappedCustomTypes(t *testing.T) {
	for _, typ := range []string{"custom.Value", "*custom.Value", "sql.Null[custom.Value]", "[]custom.Value"} {
		in := Input{Package: "db", Namer: NewNamer(),
			TypeImports: map[string]string{"custom.Value": "example.com/custom"},
			Files: []SrcFile{{Source: "q.sql", Out: "q.sql.go", Queries: []Query{
				{Name: "Get", Command: "one", SQL: "SELECT u FROM t",
					Ret: Ret{Kind: RetScalar, Type: typ}},
			}}}}
		out, err := Render(in)
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range out {
			if f.Name == "q.sql.go" && !strings.Contains(string(f.Data), `"example.com/custom"`) {
				t.Errorf("%s: the custom import is missing:\n%s", typ, f.Data)
			}
		}
	}
}
