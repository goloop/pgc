package catalog

import (
	"strings"
	"testing"

	"github.com/goloop/pgc/internal/pgwire"
)

// fakeQuerier serves canned rows keyed by a substring of the SQL.
type fakeQuerier struct {
	byNeedle map[string][][]pgwire.Value
}

func (f *fakeQuerier) Query(sql string) ([][]pgwire.Value, error) {
	for needle, rows := range f.byNeedle {
		if strings.Contains(sql, needle) {
			return rows, nil
		}
	}
	return nil, nil
}

func v(s string) pgwire.Value { return pgwire.Value{S: s, Valid: true} }

func TestLoad(t *testing.T) {
	q := &fakeQuerier{byNeedle: map[string][][]pgwire.Value{
		"pg_type": {
			{v("20"), v("int8"), v("b"), v("N"), v("0"), v("0")},
			{v("25"), v("text"), v("b"), v("S"), v("0"), v("0")},
		},
		"pg_class": {
			{v("100"), v("users"), v("public")},
		},
		"pg_attribute": {
			{v("100"), v("1"), v("id"), v("20"), v("t")},
			{v("100"), v("2"), v("email"), v("25"), v("t")},
			{v("100"), v("3"), v("bio"), v("25"), v("f")},
		},
	}}

	c, err := Load(q, []uint32{20, 25, 20}, []uint32{100})
	if err != nil {
		t.Fatal(err)
	}

	if name, ok := c.TypeName(20); !ok || name != "int8" {
		t.Errorf("TypeName(20) = %q %v", name, ok)
	}
	table, ok := c.Table(100)
	if !ok || table.Name != "users" || len(table.Columns) != 3 {
		t.Fatalf("table = %+v", table)
	}
	if table.Columns[2].Name != "bio" || table.Columns[2].NotNull {
		t.Errorf("bio = %+v", table.Columns[2])
	}
	if !c.NotNull(100, 1) || c.NotNull(100, 3) {
		t.Error("NotNull attnotnull mismatch")
	}
	if got := c.ColumnTypeOIDs(); len(got) != 2 || got[0] != 20 || got[1] != 25 {
		t.Errorf("ColumnTypeOIDs = %v", got)
	}
}

func TestLoadMissingType(t *testing.T) {
	q := &fakeQuerier{byNeedle: map[string][][]pgwire.Value{}}
	if _, err := Load(q, []uint32{999}, nil); err == nil {
		t.Error("want error for unknown type oid")
	}
}

func TestOIDListDedup(t *testing.T) {
	if got := oidList([]uint32{5, 0, 5, 3}); got != "3,5" {
		t.Errorf("oidList = %q", got)
	}
}
