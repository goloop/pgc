package gen

import (
	"strings"
	"testing"
)

// TestIterShape locks the :iter emission the same way the golden files lock
// the rest: the whole function body must match.
func TestIterShape(t *testing.T) {
	in := Input{
		Package: "db",
		Files: []SrcFile{{
			Source: "queries/orders.sql",
			Out:    "orders.sql.go",
			Queries: []Query{{
				Name: "IterOrders",
				Doc: "IterOrders runs the query and streams the matching rows. " +
					"Iteration stops at the first error.",
				Command: "iter",
				SQL:     "SELECT id, total FROM orders WHERE total > $1",
				Params:  []Param{{Name: "total", Type: "string"}},
				Ret: Ret{Kind: RetRow, Type: "IterOrdersRow", Fields: []Field{
					{Name: "id", Type: "int64"},
					{Name: "total", Type: "string"},
				}},
			}},
		}},
	}
	files, err := Render(in)
	if err != nil {
		t.Fatal(err)
	}
	src := string(files[len(files)-1].Data)

	want := `func (q *Queries) IterOrders(ctx context.Context, total string) iter.Seq2[IterOrdersRow, error] {
	return func(yield func(IterOrdersRow, error) bool) {
		var zero IterOrdersRow
		rows, err := q.db.QueryContext(ctx, iterOrders, total)
		if err != nil {
			yield(zero, err)
			return
		}
		defer rows.Close()

		for rows.Next() {
			var r IterOrdersRow
			if err := rows.Scan(&r.ID, &r.Total); err != nil {
				yield(zero, err)
				return
			}
			if !yield(r, nil) {
				return
			}
		}
		if err := rows.Err(); err != nil {
			yield(zero, err)
		}
	}
}`
	if !strings.Contains(src, want) {
		t.Errorf("iter emission mismatch:\n%s", src)
	}
	if !strings.Contains(src, "\t\"iter\"\n") {
		t.Errorf("missing iter import:\n%s", src)
	}
}

// TestEnumEmission locks the enum shape in models.go.
func TestEnumEmission(t *testing.T) {
	in := Input{
		Package: "db",
		Enums: []Enum{{
			Name: "OrderStatus", DBName: "order_status",
			Values: []EnumValue{
				{Name: "OrderStatusPending", Value: "pending"},
				{Name: "OrderStatusPaid", Value: "paid"},
			},
		}},
	}
	files, err := Render(in)
	if err != nil {
		t.Fatal(err)
	}
	var models string
	for _, f := range files {
		if f.Name == "models.go" {
			models = string(f.Data)
		}
	}

	want := `// OrderStatus mirrors the PostgreSQL enum order_status.
type OrderStatus string

// The order_status values.
const (
	OrderStatusPending OrderStatus = "pending"
	OrderStatusPaid    OrderStatus = "paid"
)`
	if !strings.Contains(models, want) {
		t.Errorf("enum emission mismatch:\n%s", models)
	}
}
