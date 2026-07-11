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
