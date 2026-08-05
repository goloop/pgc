package compile

import "testing"

// TestNeverNull is the whole safety argument of the feature: the list is short
// and everything outside it stays nullable. A false positive here is a NULL
// arriving at run time in a value that cannot hold one.
func TestNeverNull(t *testing.T) {
	proven := []string{
		"count(*)",
		"COUNT(*)",
		"count(id)",
		"count(distinct author_id)",
		"count(*) AS total",
		"count(*) total",
		"coalesce(sum(total), 0)",
		"coalesce(name, '')",
		"coalesce(a, b, 'x')",
		"coalesce(name, '') AS name",
		"'draft'",
		"'draft'::text",
		"0",
		"0::bigint",
		"'{}'::jsonb",
		"0::numeric(10,2)",
		"'x'::pg_catalog.text",
		"0::double precision",
		"coalesce(a, 0::int8)",
		"-1",
		"true",
		"FALSE",
	}
	for _, e := range proven {
		if !neverNull(e) {
			t.Errorf("neverNull(%q) = false, want true", e)
		}
	}

	unproven := []string{
		// Aggregates that really do return NULL for no rows.
		"max(created_at)",
		"sum(total)",
		"avg(score)",
		"min(id)",

		// coalesce is only total when its last argument is.
		"coalesce(a, b)",
		"coalesce(name, other_name)",
		"coalesce(a, NULL)",
		"coalesce(a, null)",

		// Not the call it looks like.
		"count(*) + 1",
		"1 + count(*)",
		"nullif(count(*), 0)",
		"f(coalesce(a, 'b'))",
		"discount(x)",
		"my_count(*)",

		// Plain columns and everything else.
		"author_id",
		"a + b",
		"NULL",
		"null::text",

		// A cast to a user-defined type runs a user-defined function, which
		// may be non-strict and return NULL for a non-null input.
		"1::my_type",
		"'x'::money_like",
		"coalesce(a, 1::my_type)",
		"(SELECT count(*) FROM t)",
		"",
		"   ",
	}
	for _, e := range unproven {
		if neverNull(e) {
			t.Errorf("neverNull(%q) = true, want false", e)
		}
	}
}

// TestExpressionNotNull covers the mapping from a column position to its
// expression, which is where a wrong answer would land on the wrong column.
func TestExpressionNotNull(t *testing.T) {
	cases := []struct {
		name    string
		sql     string
		columns int
		want    []bool // one per column
	}{
		{
			name:    "a facet query",
			sql:     "SELECT status, count(*) AS n FROM articles GROUP BY status",
			columns: 2,
			want:    []bool{false, true},
		},
		{
			name:    "a lone count",
			sql:     "SELECT count(*) FROM articles",
			columns: 1,
			want:    []bool{true},
		},
		{
			name:    "coalesce next to a nullable aggregate",
			sql:     "SELECT coalesce(sum(x), 0), max(y) FROM t",
			columns: 2,
			want:    []bool{true, false},
		},
		{
			name:    "a returning list",
			sql:     "INSERT INTO t (a) VALUES ($1) RETURNING id, count_cache, 'ok'::text",
			columns: 3,
			want:    []bool{false, false, true},
		},
		{
			name:    "a comma inside a call does not split the list",
			sql:     "SELECT coalesce(a, 'x'), count(*) FROM t",
			columns: 2,
			want:    []bool{true, true},
		},
		{
			name:    "a comma inside a string does not split the list",
			sql:     "SELECT 'a, b'::text, count(*) FROM t",
			columns: 2,
			want:    []bool{true, true},
		},

		// Everything below must infer nothing at all.
		{
			name:    "a wildcard breaks the position mapping",
			sql:     "SELECT *, count(*) OVER () FROM t",
			columns: 4,
			want:    []bool{false, false, false, false},
		},
		{
			name:    "a qualified wildcard too",
			sql:     "SELECT t.*, count(*) FROM t",
			columns: 3,
			want:    []bool{false, false, false},
		},
		{
			name:    "a set operation describes combined columns",
			sql:     "SELECT count(*) FROM a UNION ALL SELECT count(*) FROM b",
			columns: 1,
			want:    []bool{false},
		},
		{
			name:    "a CTE puts the real list somewhere else",
			sql:     "WITH x AS (SELECT 1) SELECT count(*) FROM x",
			columns: 1,
			want:    []bool{false},
		},
		{
			name:    "DISTINCT ON adds an expression with no column",
			sql:     "SELECT DISTINCT ON (a) count(*) FROM t",
			columns: 1,
			want:    []bool{false},
		},
		{
			name:    "a list that does not line up is not used",
			sql:     "SELECT count(*) FROM t",
			columns: 2,
			want:    []bool{false, false},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			for i, want := range c.want {
				if got := expressionNotNull(c.sql, i, c.columns); got != want {
					t.Errorf("column %d: expressionNotNull = %v, want %v", i, got, want)
				}
			}
		})
	}
}

// TestNeverNullPlainDistinct checks a plain DISTINCT stays transparent - only
// DISTINCT ON moves the list.
func TestNeverNullPlainDistinct(t *testing.T) {
	sql := "SELECT DISTINCT count(*) FROM t"
	if !expressionNotNull(sql, 0, 1) {
		t.Errorf("a plain DISTINCT should not stop the inference: %s", sql)
	}
}
