package queryfile

import "testing"

func infer(t *testing.T, sql string, count int) map[int]string {
	t.Helper()
	return InferParamNames(Query{SQL: sql, ParamNames: map[int]string{}}, count)
}

func TestInferComparisons(t *testing.T) {
	names := infer(t,
		`SELECT * FROM users WHERE id = $1 AND email LIKE $2 AND $3 <= age`, 3)
	want := map[int]string{1: "id", 2: "email", 3: "age"}
	for n, w := range want {
		if names[n] != w {
			t.Errorf("$%d = %q, want %q", n, names[n], w)
		}
	}
}

func TestInferQualifiedAndIn(t *testing.T) {
	names := infer(t,
		`SELECT * FROM users u WHERE u.tenant_id = $1 AND status IN ($2)`, 2)
	if names[1] != "tenant_id" || names[2] != "status" {
		t.Errorf("names = %v", names)
	}
}

func TestInferInsertValues(t *testing.T) {
	names := infer(t, `INSERT INTO users (email, name, age)
VALUES ($1, $2, $3)
RETURNING id`, 3)
	if names[1] != "email" || names[2] != "name" || names[3] != "age" {
		t.Errorf("names = %v", names)
	}
}

func TestInferInsertExpressionArg(t *testing.T) {
	// lower($2) is not a bare parameter: position 2 must fall back.
	names := infer(t, `INSERT INTO t (a, b) VALUES ($1, lower($2))`, 2)
	if names[1] != "a" || names[2] != "arg2" {
		t.Errorf("names = %v", names)
	}
}

func TestInferLimitOffsetAndFallback(t *testing.T) {
	names := infer(t, `SELECT id FROM t WHERE x > now() - $3
ORDER BY id LIMIT $1 OFFSET $2`, 3)
	if names[1] != "limit" || names[2] != "offset" || names[3] != "arg3" {
		t.Errorf("names = %v", names)
	}
}

func TestInferAnnotationWins(t *testing.T) {
	q := Query{
		SQL:        `SELECT * FROM t WHERE id = $1`,
		ParamNames: map[int]string{1: "user_ref"},
	}
	names := InferParamNames(q, 1)
	if names[1] != "user_ref" {
		t.Errorf("names = %v", names)
	}
}

func TestTokenizerSkipsLiteralsAndComments(t *testing.T) {
	names := infer(t, `SELECT
 -- id = $9 inside a comment
 '$9 in a string', "weird""?", $$body with $1$$, e = $1
FROM t`, 1)
	if names[1] != "e" {
		t.Errorf("names = %v", names)
	}
}

func TestInferAnyBothDirections(t *testing.T) {
	names := infer(t,
		`SELECT * FROM notes WHERE id = ANY($1) AND $2 = ANY(tags)`, 2)
	if names[1] != "id" || names[2] != "tags" {
		t.Errorf("names = %v", names)
	}
	// ANY/ALL themselves must never become a parameter name.
	names = infer(t, `SELECT * FROM t WHERE $1 = ANY(SELECT x FROM u)`, 1)
	if names[1] != "arg1" {
		t.Errorf("names = %v", names)
	}
}

func TestInferUpdateSet(t *testing.T) {
	names := infer(t, `UPDATE users SET name = $2, email = $3 WHERE id = $1`, 3)
	if names[1] != "id" || names[2] != "name" || names[3] != "email" {
		t.Errorf("names = %v", names)
	}
}

// A quoted identifier with a doubled (escaped) quote is read whole, so the
// closing quote is not mistaken for the first inner quote.
func TestReadQuotedDoubledQuote(t *testing.T) {
	val, rest := readQuoted(`"user""id" = $1`)
	if val != `user"id` {
		t.Errorf("val = %q, want %q", val, `user"id`)
	}
	if rest != " = $1" {
		t.Errorf("rest = %q, want %q", rest, " = $1")
	}
}

// A quoted identifier is a column however it is spelled: "order", "group" or
// "limit" in double quotes must name the parameter next to them, while the
// same words unquoted stay keywords. Every neighbour pattern is covered, as is
// the INSERT column list.
func TestInferQuotedKeywordIsAColumn(t *testing.T) {
	cases := []struct {
		sql  string
		n    int
		want map[int]string
	}{
		{`UPDATE pages SET "order" = $1 WHERE id = $2`, 2,
			map[int]string{1: "order", 2: "id"}},
		{`UPDATE pages SET parent_id = $3, "order" = $4 WHERE id = $1 AND document_id = $2`, 4,
			map[int]string{1: "id", 2: "document_id", 3: "parent_id", 4: "order"}},
		{`SELECT 1 FROM t WHERE $1 = "order"`, 1, map[int]string{1: "order"}},
		{`SELECT 1 FROM t p WHERE p."order" = $1`, 1, map[int]string{1: "order"}},
		{`SELECT 1 FROM t WHERE "group" IN ($1)`, 1, map[int]string{1: "group"}},
		{`SELECT 1 FROM t WHERE "order" = ANY($1)`, 1, map[int]string{1: "order"}},
		{`SELECT 1 FROM t WHERE $1 = ANY("all")`, 1, map[int]string{1: "all"}},
		{`SELECT 1 FROM t WHERE "end" LIKE $1`, 1, map[int]string{1: "end"}},
		{`SELECT 1 FROM t WHERE "limit" = $1 LIMIT $2`, 2, map[int]string{1: "limit", 2: "limit"}},
		{`INSERT INTO t ("order", "values") VALUES ($1, $2)`, 2, map[int]string{1: "order", 2: "values"}},
		{`INSERT INTO "values" ("in") VALUES ($1)`, 1, map[int]string{1: "in"}},
		// A quoted identifier keeps its spelling; the Go name is the generator's job.
		{`SELECT 1 FROM t WHERE "createdAt" = $1`, 1, map[int]string{1: "createdAt"}},
		// Unquoted keywords are still rejected: this is what isKeyword is for.
		{`SELECT 1 FROM t ORDER BY id LIMIT $1`, 1, map[int]string{1: "limit"}},
		{`SELECT 1 FROM t WHERE x IS NOT NULL AND $1 = ANY(SELECT x FROM u)`, 1, map[int]string{1: "arg1"}},
		// A quoted "values" is not the VALUES keyword, so the INSERT is not
		// matched through it.
		{`INSERT INTO t (a) SELECT "values" FROM u WHERE "values" ($1) IS NULL`, 1, map[int]string{1: "arg1"}},
	}
	for _, c := range cases {
		got := infer(t, c.sql, c.n)
		for k, v := range c.want {
			if got[k] != v {
				t.Errorf("%s: $%d = %q, want %q", c.sql, k, got[k], v)
			}
		}
	}
}

// On the right-hand side of a comparison the scan starts at the qualifier,
// so a qualified column must be walked to its last part: $1 = p.id names the
// parameter id, not p.
func TestInferQualifiedRightHandSide(t *testing.T) {
	names := infer(t,
		`SELECT 1 FROM t p JOIN u ON true WHERE $1 = p."order" AND $2 = ANY(p.tags) AND $3 = u.id AND $4 = s.u.col`, 4)
	want := map[int]string{1: "order", 2: "tags", 3: "id", 4: "col"}
	for n, w := range want {
		if names[n] != w {
			t.Errorf("$%d = %q, want %q (names = %v)", n, names[n], w, names)
		}
	}
}
