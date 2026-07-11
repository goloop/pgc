package compile

import (
	"strings"
	"testing"

	"github.com/goloop/pgc/internal/pgwire"
)

// featuresDB serves a catalog exercising domains, arrays, schemas and
// joins:
//
//	public.users : id int8 NOT NULL, email email_dom (domain over text)
//	               NOT NULL, tags _int4 (integer[]) NULL
//	public.orders: id int8 NOT NULL, user_id int8 NOT NULL
//	audit.users  : id int8 NOT NULL
type featuresDB struct {
	statements map[string]*pgwire.Statement
}

func (f *featuresDB) Describe(query string) (*pgwire.Statement, error) {
	st, ok := f.statements[query]
	if !ok {
		return nil, &pgwire.ServerError{
			Severity: "ERROR", Code: "42601", Message: "no such fixture",
		}
	}
	return st, nil
}

func (f *featuresDB) Query(sql string) ([][]pgwire.Value, error) {
	v := func(s string) pgwire.Value { return pgwire.Value{S: s, Valid: true} }
	switch {
	case strings.Contains(sql, "pg_enum"):
		return nil, nil
	case strings.Contains(sql, "pg_type"):
		return [][]pgwire.Value{
			{v("20"), v("int8"), v("b"), v("N"), v("0"), v("0")},
			{v("23"), v("int4"), v("b"), v("N"), v("0"), v("0")},
			{v("25"), v("text"), v("b"), v("S"), v("0"), v("0")},
			{v("400"), v("email_dom"), v("d"), v("S"), v("0"), v("25")},
			{v("1007"), v("_int4"), v("b"), v("A"), v("23"), v("0")},
		}, nil
	case strings.Contains(sql, "pg_attribute"):
		return [][]pgwire.Value{
			{v("100"), v("1"), v("id"), v("20"), v("t")},
			{v("100"), v("2"), v("email"), v("400"), v("t")},
			{v("100"), v("3"), v("tags"), v("1007"), v("f")},
			{v("200"), v("1"), v("id"), v("20"), v("t")},
			{v("200"), v("2"), v("user_id"), v("20"), v("t")},
			{v("210"), v("1"), v("id"), v("20"), v("t")},
		}, nil
	case strings.Contains(sql, "pg_class"):
		return [][]pgwire.Value{
			{v("100"), v("users"), v("public")},
			{v("200"), v("orders"), v("public")},
			{v("210"), v("users"), v("audit")},
		}, nil
	}
	return nil, nil
}

func col(table uint32, attnum int16, name string, typ uint32) pgwire.Column {
	return pgwire.Column{Name: name, TypeOID: typ, TableOID: table, Attnum: attnum}
}

func usersFull(t uint32) []pgwire.Column {
	return []pgwire.Column{
		col(t, 1, "id", 20), col(t, 2, "email", 400), col(t, 3, "tags", 1007),
	}
}

func TestDomainArrayAndCustomType(t *testing.T) {
	sql := "SELECT id, email, tags FROM users WHERE id = ANY($1)"
	custom := "SELECT id FROM users WHERE email = $1"
	dir := writeQueries(t, `-- name: FindUsers :many
`+sql+`;

-- name: FindByUUID :many
-- override: $1 github.com/google/uuid.UUID
`+custom+`;
`)

	db := &featuresDB{statements: map[string]*pgwire.Statement{
		sql: {
			ParamOIDs: []uint32{1007},
			Columns:   usersFull(100),
		},
		custom: {
			ParamOIDs: []uint32{25},
			Columns:   []pgwire.Column{col(100, 1, "id", 20)},
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

	models := byName["models.go"]
	for _, want := range []string{
		"Email string",  // domain resolved to its base
		"Tags  []int32", // nullable array stays a plain slice
	} {
		if !strings.Contains(models, want) {
			t.Errorf("models.go missing %q:\n%s", want, models)
		}
	}

	users := byName["users.sql.go"]
	for _, want := range []string{
		"func (q *Queries) FindUsers(ctx context.Context, id []int32) ([]User, error) {",
		"q.db.QueryContext(ctx, findUsers, int32Array(id))",
		"(*int32Array)(&u.Tags)",
		"func (q *Queries) FindByUUID(ctx context.Context, email uuid.UUID) ([]int64, error) {",
		"\"github.com/google/uuid\"",
	} {
		if !strings.Contains(users, want) {
			t.Errorf("users.sql.go missing %q:\n%s", want, users)
		}
	}

	arrays, ok := byName["pgarray.go"]
	if !ok {
		t.Fatal("pgarray.go was not emitted")
	}
	for _, want := range []string{
		"type int32Array []int32",
		"func (a *int32Array) Scan(src any) error {",
		"func (a int32Array) Value() (driver.Value, error) {",
		"func pgArrayElems(src any, who string) ([]string, error) {",
	} {
		if !strings.Contains(arrays, want) {
			t.Errorf("pgarray.go missing %q:\n%s", want, arrays)
		}
	}
}

func TestSchemaCollisionAndRename(t *testing.T) {
	pub := "SELECT id, email, tags FROM users"
	aud := "SELECT id FROM audit.users FOR UPDATE"
	src := `-- name: PublicUsers :many
` + pub + `;

-- name: AuditUsers :many
-- embed: audit.users
` + aud + `;
`
	statements := map[string]*pgwire.Statement{
		pub: {Columns: usersFull(100)},
		aud: {Columns: []pgwire.Column{col(210, 1, "id", 20)}},
	}

	// Without a rename both tables become "Users": a collision.
	dir := writeQueries(t, src)
	cfg := testConfig(dir)
	cfg.Rename = nil
	if _, err := Run(&featuresDB{statements: statements}, cfg); err == nil ||
		!strings.Contains(err.Error(), "collides") {
		t.Fatalf("want collision error, got %v", err)
	}

	// A schema-qualified rename resolves it.
	cfg.Rename = map[string]string{"audit.users": "AuditUser"}
	res, err := Run(&featuresDB{statements: statements}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	var models string
	for _, f := range res.Files {
		if f.Name == "models.go" {
			models = string(f.Data)
		}
	}
	if !strings.Contains(models, "// AuditUser mirrors one row of the audit.users table.") {
		t.Errorf("schema-qualified model doc missing:\n%s", models)
	}
}

func TestEmbed(t *testing.T) {
	sql := "SELECT o.id, o.user_id, u.id, u.email, u.tags " +
		"FROM orders o JOIN users u ON u.id = o.user_id"
	dir := writeQueries(t, `-- name: OrdersWithUser :many
-- embed: public.users as Buyer
`+sql+`;
`)

	db := &featuresDB{statements: map[string]*pgwire.Statement{
		sql: {Columns: append(
			[]pgwire.Column{col(200, 1, "id", 20), col(200, 2, "user_id", 20)},
			usersFull(100)...)},
	}}
	res, err := Run(db, testConfig(dir))
	if err != nil {
		t.Fatal(err)
	}
	var users string
	for _, f := range res.Files {
		if f.Name == "users.sql.go" {
			users = string(f.Data)
		}
	}
	for _, want := range []string{
		"type OrdersWithUserRow struct {",
		"Buyer  User",
		"rows.Scan(&r.ID, &r.UserID, &r.Buyer.ID, &r.Buyer.Email, (*int32Array)(&r.Buyer.Tags))",
	} {
		if !strings.Contains(users, want) {
			t.Errorf("missing %q:\n%s", want, users)
		}
	}
}

func TestEmbedNoRun(t *testing.T) {
	sql := "SELECT o.id, u.email FROM orders o JOIN users u ON true"
	dir := writeQueries(t, "-- name: Partial :many\n-- embed: public.users\n"+sql+";\n")

	db := &featuresDB{statements: map[string]*pgwire.Statement{
		sql: {Columns: []pgwire.Column{
			col(200, 1, "id", 20), col(100, 2, "email", 400),
		}},
	}}
	if _, err := Run(db, testConfig(dir)); err == nil ||
		!strings.Contains(err.Error(), "full column list") {
		t.Fatalf("want embed-run error, got %v", err)
	}
}

func TestJSONTagsAndQuerier(t *testing.T) {
	sql := "SELECT id, email, bio FROM users WHERE id = $1"
	dir := writeQueries(t, "-- name: GetUser :one\n"+sql+";\n")

	db := &fakeDB{statements: map[string]*pgwire.Statement{
		sql: {
			ParamOIDs: []uint32{20},
			Columns: []pgwire.Column{
				usersCol(1, "id", 20), usersCol(2, "email", 25), usersCol(3, "bio", 25),
			},
		},
	}}
	cfg := testConfig(dir)
	cfg.JSONTags = true
	cfg.Interface = true

	res, err := Run(db, cfg)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]string{}
	for _, f := range res.Files {
		byName[f.Name] = string(f.Data)
	}

	if !strings.Contains(byName["models.go"], "Bio   *string `json:\"bio\"`") {
		t.Errorf("json tag missing:\n%s", byName["models.go"])
	}
	querier, ok := byName["querier.go"]
	if !ok {
		t.Fatal("querier.go was not emitted")
	}
	for _, want := range []string{
		"type Querier interface {",
		"GetUser(ctx context.Context, id int64) (User, error)",
		"var _ Querier = (*Queries)(nil)",
	} {
		if !strings.Contains(querier, want) {
			t.Errorf("querier.go missing %q:\n%s", want, querier)
		}
	}
}

// A querier must not import packages that only row struct fields use: the
// interface mentions the structs by name alone.
func TestQuerierImportsOnlySignatureTypes(t *testing.T) {
	sql := "SELECT id, email FROM users ORDER BY id"
	dir := writeQueries(t, "-- name: Pairs :many\n"+sql+";\n")

	db := &featuresDB{statements: map[string]*pgwire.Statement{
		sql: {Columns: []pgwire.Column{
			// tags is _int4: the row struct field needs the array helper,
			// but the querier signature only says PairsRow.
			col(100, 1, "id", 20), col(100, 3, "tags", 1007),
		}},
	}}
	cfg := testConfig(dir)
	cfg.Interface = true
	res, err := Run(db, cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range res.Files {
		if f.Name != "querier.go" {
			continue
		}
		src := string(f.Data)
		if strings.Contains(src, "\"time\"") || strings.Contains(src, "encoding/json") {
			t.Errorf("querier.go pulls field-type imports:\n%s", src)
		}
		if !strings.Contains(src, `import "context"`) {
			t.Errorf("querier.go must still import context:\n%s", src)
		}
	}
}
