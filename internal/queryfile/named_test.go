package queryfile

import (
	"strings"
	"testing"
)

// TestRewriteNamed covers the numbering rules and, more importantly, every
// place an "@" is not a parameter. Rewriting one of those would corrupt the
// statement silently.
func TestRewriteNamed(t *testing.T) {
	cases := []struct {
		name  string
		sql   string
		want  string
		names []string
	}{
		{
			name:  "numbered by first appearance",
			sql:   "INSERT INTO t (a, b) VALUES (@title, @slug)",
			want:  "INSERT INTO t (a, b) VALUES ($1, $2)",
			names: []string{"title", "slug"},
		},
		{
			name:  "a repeated name reuses its number",
			sql:   "WHERE lower(a) = @q OR lower(b) = @q OR c = @limit",
			want:  "WHERE lower(a) = $1 OR lower(b) = $1 OR c = $2",
			names: []string{"q", "limit"},
		},
		{
			name:  "a cast follows the name",
			sql:   "SELECT @id::bigint, @name::text",
			want:  "SELECT $1::bigint, $2::text",
			names: []string{"id", "name"},
		},
		{
			name:  "underscores and digits are part of the name",
			sql:   "WHERE a = @user_id2 AND b = @_x",
			want:  "WHERE a = $1 AND b = $2",
			names: []string{"user_id2", "_x"},
		},

		// Everything below must come back byte for byte.
		{
			name: "the containment operator is not a parameter",
			sql:  "WHERE tags @> ARRAY['a'] AND meta <@ other",
			want: "WHERE tags @> ARRAY['a'] AND meta <@ other",
		},
		{
			name: "text search operators are not parameters",
			sql:  "WHERE doc @@ to_tsquery('en', 'cat')",
			want: "WHERE doc @@ to_tsquery('en', 'cat')",
		},
		{
			name: "an address inside a string literal",
			sql:  "SELECT 'user@example.com'",
			want: "SELECT 'user@example.com'",
		},
		{
			name: "a doubled quote does not end the string",
			sql:  "SELECT 'it''s @name here'",
			want: "SELECT 'it''s @name here'",
		},
		{
			name: "an escape string keeps its backslash quote",
			sql:  `SELECT E'a\' @name b'`,
			want: `SELECT E'a\' @name b'`,
		},
		{
			name: "a quoted identifier",
			sql:  `SELECT "@weird" FROM t`,
			want: `SELECT "@weird" FROM t`,
		},
		{
			name: "a line comment",
			sql:  "SELECT 1 -- @note\nFROM t",
			want: "SELECT 1 -- @note\nFROM t",
		},
		{
			name: "a nested block comment",
			sql:  "SELECT /* @a /* @b */ @c */ 1",
			want: "SELECT /* @a /* @b */ @c */ 1",
		},
		{
			name: "a dollar-quoted body",
			sql:  "SELECT $$ @name $$",
			want: "SELECT $$ @name $$",
		},
		{
			name: "a tagged dollar-quoted body",
			sql:  "SELECT $tag$ @name $tag$",
			want: "SELECT $tag$ @name $tag$",
		},
		{
			name: "a bare at sign",
			sql:  "SELECT @ 1",
			want: "SELECT @ 1",
		},
		{
			name: "positional parameters are left alone",
			sql:  "WHERE a = $1 AND b = $2",
			want: "WHERE a = $1 AND b = $2",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, names, err := rewriteNamed(c.sql)
			if err != nil {
				t.Fatalf("rewriteNamed() = %v", err)
			}
			if got != c.want {
				t.Errorf("sql:\n got %q\nwant %q", got, c.want)
			}
			if strings.Join(names, ",") != strings.Join(c.names, ",") {
				t.Errorf("names = %v, want %v", names, c.names)
			}
		})
	}
}

// TestRewriteNamedRejectsMixing pins the one combination that is refused: a
// query that both names and numbers its parameters puts the reader back to
// counting, which is the thing names exist to avoid.
func TestRewriteNamedRejectsMixing(t *testing.T) {
	_, _, err := rewriteNamed("WHERE a = @id AND b = $2")
	if err == nil {
		t.Fatal("mixing @name with $N was accepted")
	}
	if !strings.Contains(err.Error(), "@id") {
		t.Errorf("error does not name the parameter: %v", err)
	}
}

// TestRewriteNamedUnterminated checks a half-written literal cannot make the
// scanner run past the end of the input. The server reports the syntax error;
// this only has to survive reaching it.
func TestRewriteNamedUnterminated(t *testing.T) {
	for _, sql := range []string{
		"SELECT 'unclosed @name",
		`SELECT "unclosed @name`,
		"SELECT /* unclosed @name",
		"SELECT $$ unclosed @name",
		"SELECT $tag$ unclosed @name",
		"SELECT 1 -- trailing @name",
		"@",
		"",
	} {
		if _, _, err := rewriteNamed(sql); err != nil {
			t.Errorf("rewriteNamed(%q) = %v", sql, err)
		}
	}
}

// TestNamedParamsThroughParseFile is the end-to-end shape: the body reaches
// the server positional, and the names arrive where the generator reads them.
func TestNamedParamsThroughParseFile(t *testing.T) {
	src := `-- name: CreateArticle :one
-- CreateArticle inserts an article.
INSERT INTO articles (title, slug, body, author_id)
VALUES (@title, @slug, @body, @author_id)
RETURNING id;
`
	qs, err := ParseFile("q.sql", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	q := qs[0]

	if strings.Contains(q.SQL, "@") {
		t.Errorf("named placeholders reached the server:\n%s", q.SQL)
	}
	if !strings.Contains(q.SQL, "VALUES ($1, $2, $3, $4)") {
		t.Errorf("body was not renumbered:\n%s", q.SQL)
	}
	want := map[int]string{1: "title", 2: "slug", 3: "body", 4: "author_id"}
	for n, name := range want {
		if q.ParamNames[n] != name {
			t.Errorf("ParamNames[%d] = %q, want %q", n, q.ParamNames[n], name)
		}
	}
}

// TestNamedOverride checks an override can name the parameter too. Having to
// write "$14" in an annotation is the counting the names removed.
func TestNamedOverride(t *testing.T) {
	src := `-- name: Search :many
-- override: @tags []string
-- override: id int64 notnull
SELECT id FROM t WHERE tags = @tags AND owner = @owner;
`
	qs, err := ParseFile("q.sql", []byte(src))
	if err != nil {
		t.Fatal(err)
	}

	var found bool
	for _, o := range qs[0].Overrides {
		if o.GoType == "[]string" {
			found = true
			if o.Param != 1 {
				t.Errorf("override resolved to $%d, want $1", o.Param)
			}
			if o.named != "" {
				t.Errorf("the name was left unresolved: %q", o.named)
			}
		}
	}
	if !found {
		t.Error("the named override was lost")
	}
}

// TestNamedAnnotationConflicts covers the annotations that stop making sense
// once the parameters name themselves.
func TestNamedAnnotationConflicts(t *testing.T) {
	cases := map[string]string{
		"param annotation alongside names": `-- name: A :one
-- param: $1 other
SELECT @id;
`,
		"override names a parameter that is not there": `-- name: A :one
-- override: @nope int64
SELECT @id;
`,
		"override names a parameter in a positional query": `-- name: A :one
-- override: @nope int64
SELECT $1;
`,
		"mixed styles": `-- name: A :one
SELECT @id, $2;
`,
	}

	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseFile("q.sql", []byte(src)); err == nil {
				t.Error("accepted without complaint")
			}
		})
	}
}

// TestNamedIdentifierWithDollar guards a shape that must not be mistaken for a
// dollar-quoted body: PostgreSQL identifiers may contain "$", and treating an
// unbalanced tag as a body would swallow the rest of the statement.
func TestNamedIdentifierWithDollar(t *testing.T) {
	sql := "SELECT price$usd$total FROM t WHERE id = @id"
	got, names, err := rewriteNamed(sql)
	if err != nil {
		t.Fatal(err)
	}
	if want := "SELECT price$usd$total FROM t WHERE id = $1"; got != want {
		t.Errorf("sql:\n got %q\nwant %q", got, want)
	}
	if len(names) != 1 || names[0] != "id" {
		t.Errorf("names = %v, want [id]", names)
	}
}

// TestResolveNamedLeavesQueryIntactOnError checks a query that fails to
// resolve is left exactly as it was read, rather than with a body and a
// parameter map that disagree.
func TestResolveNamedLeavesQueryIntactOnError(t *testing.T) {
	q := &Query{
		Name:       "A",
		SQL:        "SELECT @id",
		ParamNames: map[int]string{},
		Overrides:  []Override{{named: "nope", GoType: "int64"}},
	}
	before := q.SQL

	if err := resolveNamed(q); err == nil {
		t.Fatal("an override naming an absent parameter was accepted")
	}
	if q.SQL != before {
		t.Errorf("body was rewritten anyway: %q", q.SQL)
	}
	if len(q.ParamNames) != 0 {
		t.Errorf("names were recorded anyway: %v", q.ParamNames)
	}
	if q.Overrides[0].named != "nope" {
		t.Errorf("override was half resolved: %+v", q.Overrides[0])
	}
}

// TestNamedUnicode checks a byte-wise scan does not trip over multibyte text
// in the places it has to skip.
func TestNamedUnicode(t *testing.T) {
	sql := "SELECT 'осінь @настрій' /* коментар @x */ FROM t WHERE назва = @q"
	got, names, err := rewriteNamed(sql)
	if err != nil {
		t.Fatal(err)
	}
	want := "SELECT 'осінь @настрій' /* коментар @x */ FROM t WHERE назва = $1"
	if got != want {
		t.Errorf("sql:\n got %q\nwant %q", got, want)
	}
	if len(names) != 1 || names[0] != "q" {
		t.Errorf("names = %v, want [q]", names)
	}
}
