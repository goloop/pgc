package queryfile

import (
	"strings"
	"testing"
)

// TestSelectList checks the split, and above all the cases it must refuse:
// a list that lines up with the columns by luck would attach one column's
// decision to another.
func TestSelectList(t *testing.T) {
	cases := []struct {
		name  string
		sql   string
		items []string
		ok    bool
	}{
		{
			name:  "a plain list",
			sql:   "SELECT id, name FROM users",
			items: []string{"id", "name"},
			ok:    true,
		},
		{
			name:  "commas inside a call stay inside it",
			sql:   "SELECT coalesce(a, b, 'x'), count(*) FROM t",
			items: []string{"coalesce(a, b, 'x')", "count(*)"},
			ok:    true,
		},
		{
			name:  "a comma inside a string is not a separator",
			sql:   "SELECT 'a, b', c FROM t",
			items: []string{"'a, b'", "c"},
			ok:    true,
		},
		{
			name:  "a comma inside a comment is not one either",
			sql:   "SELECT a /* x, y */, b FROM t",
			items: []string{"a /* x, y */", "b"},
			ok:    true,
		},
		{
			name:  "no FROM at all",
			sql:   "SELECT 1, 2",
			items: []string{"1", "2"},
			ok:    true,
		},
		{
			name:  "a FROM inside a subquery is not the end of the list",
			sql:   "SELECT (SELECT count(*) FROM b), a FROM t",
			items: []string{"(SELECT count(*) FROM b)", "a"},
			ok:    true,
		},
		{
			name:  "a plain DISTINCT is transparent",
			sql:   "SELECT DISTINCT a, b FROM t",
			items: []string{"a", "b"},
			ok:    true,
		},
		{
			name:  "a returning list",
			sql:   "INSERT INTO t (a) VALUES ($1) RETURNING id, created_at",
			items: []string{"id", "created_at"},
			ok:    true,
		},
		{
			name:  "an update returning list",
			sql:   "UPDATE t SET a = $1 WHERE id = $2 RETURNING id",
			items: []string{"id"},
			ok:    true,
		},

		{name: "a wildcard", sql: "SELECT * FROM t"},
		{name: "a qualified wildcard", sql: "SELECT t.*, a FROM t"},
		{name: "a set operation", sql: "SELECT a FROM x UNION SELECT b FROM y"},
		{name: "an except", sql: "SELECT a FROM x EXCEPT SELECT b FROM y"},
		{name: "a CTE", sql: "WITH x AS (SELECT 1) SELECT a FROM x"},
		{name: "DISTINCT ON", sql: "SELECT DISTINCT ON (a) b FROM t"},
		{name: "an insert with no returning", sql: "INSERT INTO t (a) VALUES ($1)"},
		{name: "not a statement this understands", sql: "TABLE t"},
		{name: "empty", sql: ""},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			items, ok := SelectList(c.sql)
			if ok != c.ok {
				t.Fatalf("ok = %v, want %v (items %q)", ok, c.ok, items)
			}
			if !ok {
				return
			}
			var trimmed []string
			for _, it := range items {
				trimmed = append(trimmed, strings.TrimSpace(it))
			}
			if strings.Join(trimmed, "|") != strings.Join(c.items, "|") {
				t.Errorf("items = %q, want %q", trimmed, c.items)
			}
		})
	}
}

// TestSelectListUnionInsideSubquery checks a set operation that is not the
// statement's own does not disqualify the list.
func TestSelectListUnionInsideSubquery(t *testing.T) {
	sql := "SELECT (SELECT a FROM x UNION SELECT b FROM y), c FROM t"
	items, ok := SelectList(sql)
	if !ok {
		t.Fatal("a nested union should not stop the split")
	}
	if len(items) != 2 {
		t.Errorf("items = %q, want 2", items)
	}
}
