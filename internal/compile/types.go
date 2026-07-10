package compile

import (
	"fmt"
	"strings"

	"github.com/goloop/pgc/internal/gen"
	"github.com/goloop/pgc/internal/queryfile"
)

// Thin aliases so the rest of the package reads without long qualifiers.
type qfQuery = queryfile.Query

func queryfileParse(dir string) ([]qfQuery, error) {
	queries, err := queryfile.ParseDir(dir)
	if err != nil {
		return nil, err
	}
	seen := map[string]string{}
	for _, q := range queries {
		where := fmt.Sprintf("%s:%d", q.File, q.Line)
		if prev, ok := seen[q.Name]; ok {
			return nil, fmt.Errorf(
				"%s: query %s is already defined at %s", where, q.Name, prev)
		}
		seen[q.Name] = where
	}
	return queries, nil
}

func inferParamNames(q qfQuery, count int) map[int]string {
	return queryfile.InferParamNames(q, count)
}

// pgTypes maps a pg_type name to the Go type used for a NOT NULL value.
// Types the standard library has no natural counterpart for (uuid, numeric,
// interval) default to string; the "types" map in pgc.json overrides any
// entry, and "-- override:" adjusts a single column or parameter.
var pgTypes = map[string]string{
	"bool":   "bool",
	"int2":   "int16",
	"int4":   "int32",
	"int8":   "int64",
	"oid":    "uint32",
	"float4": "float32",
	"float8": "float64",

	"text": "string", "varchar": "string", "bpchar": "string",
	"name": "string", "citext": "string",

	"bytea": "[]byte",

	"date": "time.Time", "timestamp": "time.Time", "timestamptz": "time.Time",
	"time": "string", "timetz": "string", "interval": "string",

	"json": "json.RawMessage", "jsonb": "json.RawMessage",

	"uuid": "string", "numeric": "string", "money": "string",
	"inet": "string", "cidr": "string", "macaddr": "string", "macaddr8": "string",
}

// goType resolves one type OID into a Go type expression, applying the
// nullable policy and any override.
func (c *compiler) goType(oid uint32, notNull bool, o *queryfile.Override) (string, error) {
	if o != nil {
		switch o.Null {
		case "notnull":
			notNull = true
		case "nullable":
			notNull = false
		}
	}
	if o != nil && o.GoType != "" {
		if err := checkTypeExpr(o.GoType); err != nil {
			return "", err
		}
		// An explicit Go type is taken verbatim - the author already chose
		// its nullability shape.
		return o.GoType, nil
	}

	typname, ok := c.cat.TypeName(oid)
	if !ok {
		return "", fmt.Errorf("unresolved type oid %d", oid)
	}

	expr, ok := c.cfg.Types[typname]
	if !ok {
		expr, ok = pgTypes[typname]
	}
	if !ok && c.cat.IsEnum(oid) {
		// An enum becomes a named string type with one constant per label
		// (rendered in models.go); a "types" entry above opts out.
		expr, ok = c.enumFor(oid, typname), true
	}
	if !ok {
		return "", fmt.Errorf(
			"unsupported PostgreSQL type %q; map it in pgc.json, e.g. "+
				`"types": {%q: "string"}`, typname, typname)
	}
	if err := checkTypeExpr(expr); err != nil {
		return "", err
	}

	if notNull || inherentlyNullable(expr) {
		return expr, nil
	}
	if c.cfg.Nullable == "sqlnull" {
		return "sql.Null[" + expr + "]", nil
	}
	return "*" + expr, nil
}

// enumFor registers (once) and names the generated enum type of an OID.
func (c *compiler) enumFor(oid uint32, typname string) string {
	if e, ok := c.enums[oid]; ok {
		return e.Name
	}
	name := gen.CamelCase(typname)
	e := gen.Enum{Name: name, DBName: typname}
	for _, label := range c.cat.EnumLabels(oid) {
		e.Values = append(e.Values, gen.EnumValue{
			Name:  name + gen.CamelCase(label),
			Value: label,
		})
	}
	c.enums[oid] = e
	return name
}

// inherentlyNullable reports whether an expression already encodes NULL on
// its own (nil slice or explicit pointer), so no extra wrapping is needed.
func inherentlyNullable(expr string) bool {
	return strings.HasPrefix(expr, "*") ||
		strings.HasPrefix(expr, "[]") ||
		expr == "json.RawMessage" ||
		strings.HasPrefix(expr, "sql.Null")
}

// checkTypeExpr keeps type expressions inside what the generated imports can
// satisfy: builtins plus the time, encoding/json and database/sql selectors.
func checkTypeExpr(expr string) error {
	base := strings.TrimPrefix(expr, "*")
	base = strings.TrimPrefix(base, "[]")
	if i := strings.IndexByte(base, '.'); i >= 0 {
		switch base[:i] {
		case "time", "json", "sql":
			return nil
		}
		return fmt.Errorf(
			"type %q uses a custom package; custom types come in a later phase", expr)
	}
	return nil
}

// overrideForParam finds the "-- override: $N ..." for one parameter.
func overrideForParam(q qfQuery, n int) *queryfile.Override {
	for i := range q.Overrides {
		if q.Overrides[i].Param == n {
			return &q.Overrides[i]
		}
	}
	return nil
}

// overrideForColumn finds the "-- override: <column> ..." for one column.
func overrideForColumn(q qfQuery, name string) *queryfile.Override {
	for i := range q.Overrides {
		if q.Overrides[i].Column == name {
			return &q.Overrides[i]
		}
	}
	return nil
}

// checkOverrides rejects overrides and param annotations that name a
// parameter or column the statement does not have - almost always a typo.
func checkOverrides(q qfQuery, paramCount int, columns []string) error {
	names := map[string]bool{}
	for _, name := range columns {
		names[name] = true
	}
	for _, o := range q.Overrides {
		if o.Param > paramCount {
			return fmt.Errorf(
				"override names $%d, but the statement has %d parameter(s)",
				o.Param, paramCount)
		}
		if o.Column != "" && !names[o.Column] {
			return fmt.Errorf(
				"override names column %q, which the statement does not return",
				o.Column)
		}
	}
	for n := range q.ParamNames {
		if n > paramCount {
			return fmt.Errorf(
				"param annotation names $%d, but the statement has %d parameter(s)",
				n, paramCount)
		}
	}
	return nil
}

// docFor produces the final godoc sentence. Annotation text is trusted and
// prefixed with the name; without one a plain sentence is synthesized.
func docFor(q qfQuery) string {
	if q.Doc != "" {
		doc := q.Doc
		if !strings.HasPrefix(doc, q.Name+" ") {
			doc = q.Name + " " + strings.ToLower(doc[:1]) + doc[1:]
		}
		if !strings.HasSuffix(doc, ".") {
			doc += "."
		}
		return doc
	}
	switch q.Command {
	case "one":
		return q.Name + " runs the query and returns one row. " +
			"It returns sql.ErrNoRows when no row matches."
	case "many":
		return q.Name + " runs the query and returns the matching rows."
	case "iter":
		return q.Name + " runs the query and streams the matching rows. " +
			"Iteration stops at the first error."
	case "execrows":
		return q.Name + " runs the query and returns the number of affected rows."
	default:
		return q.Name + " runs the query."
	}
}
