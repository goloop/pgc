package compile

import (
	"fmt"
	"path"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

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

// arrayElems maps a PostgreSQL element type name onto the Go element type
// and the adapter that scans and sends it.
var arrayElems = map[string]struct{ Elem, Helper string }{
	"bool":    {"bool", "boolArray"},
	"int2":    {"int16", "int16Array"},
	"int4":    {"int32", "int32Array"},
	"int8":    {"int64", "int64Array"},
	"float4":  {"float32", "float32Array"},
	"float8":  {"float64", "float64Array"},
	"text":    {"string", "stringArray"},
	"varchar": {"string", "stringArray"},
	"bpchar":  {"string", "stringArray"},
	"name":    {"string", "stringArray"},
	"citext":  {"string", "stringArray"},
	"uuid":    {"string", "stringArray"},
	"numeric": {"string", "stringArray"},
}

// goType resolves one type OID into a Go type expression plus, for arrays,
// the adapter helper name. It applies overrides, walks domains to their
// base type, expands enums and applies the nullable policy.
func (c *compiler) goType(oid uint32, notNull bool, o *queryfile.Override) (string, string, error) {
	if o != nil {
		switch o.Null {
		case "notnull":
			notNull = true
		case "nullable":
			notNull = false
		}
	}
	if o != nil && o.GoType != "" {
		// An explicit Go type is taken verbatim - the author already chose
		// its nullability shape.
		expr, err := c.resolveExpr(o.GoType)
		return expr, "", err
	}

	cur := oid
	for depth := 0; ; depth++ {
		if depth > 8 {
			return "", "", fmt.Errorf("domain chain too deep for type oid %d", oid)
		}
		t, ok := c.cat.Type(cur)
		if !ok {
			return "", "", fmt.Errorf("unresolved type oid %d", cur)
		}

		if raw, ok := c.cfg.Types[t.Name]; ok {
			expr, err := c.resolveExpr(raw)
			if err != nil {
				return "", "", err
			}
			return c.wrapNull(expr, notNull), "", nil
		}
		if expr, ok := pgTypes[t.Name]; ok {
			return c.wrapNull(expr, notNull), "", nil
		}
		if t.Kind == 'e' {
			// An enum becomes a named string type with one constant per
			// label (rendered in models.go); a "types" entry opts out.
			return c.wrapNull(c.enumFor(cur, t.Name), notNull), "", nil
		}
		if t.Category == 'A' && t.Elem != 0 {
			elemName, err := c.terminalName(t.Elem)
			if err != nil {
				return "", "", err
			}
			ae, ok := arrayElems[elemName]
			if !ok {
				return "", "", fmt.Errorf(
					"unsupported array element type %q; map the whole column "+
						"to its text form, e.g. \"types\": {%q: \"string\"}",
					elemName, t.Name)
			}
			c.helpers[ae.Helper] = true
			// A nil slice already is SQL NULL - no extra wrapping.
			return "[]" + ae.Elem, ae.Helper, nil
		}
		if t.Kind == 'd' && t.Base != 0 {
			cur = t.Base
			continue
		}
		return "", "", fmt.Errorf(
			"unsupported PostgreSQL type %q; map it in pgc.json, e.g. "+
				`"types": {%q: "string"}`, t.Name, t.Name)
	}
}

// terminalName walks domains to the terminal type's name.
func (c *compiler) terminalName(oid uint32) (string, error) {
	for depth := 0; depth <= 8; depth++ {
		t, ok := c.cat.Type(oid)
		if !ok {
			return "", fmt.Errorf("unresolved type oid %d", oid)
		}
		if t.Kind == 'd' && t.Base != 0 {
			oid = t.Base
			continue
		}
		return t.Name, nil
	}
	return "", fmt.Errorf("domain chain too deep for type oid %d", oid)
}

// wrapNull applies the configured nullable rendering.
func (c *compiler) wrapNull(expr string, notNull bool) string {
	if notNull || inherentlyNullable(expr) {
		return expr
	}
	if c.cfg.Nullable == "sqlnull" {
		return "sql.Null[" + expr + "]"
	}
	return "*" + expr
}

// resolveExpr validates a user-written Go type expression. A type from
// another module is written with its full import path -
// "github.com/you/pkg.Type" - and is rewritten to the package selector,
// with the import registered for the emitter.
func (c *compiler) resolveExpr(expr string) (string, error) {
	prefix, rest := "", expr
	for {
		switch {
		case strings.HasPrefix(rest, "*"):
			prefix, rest = prefix+"*", rest[1:]
		case strings.HasPrefix(rest, "[]"):
			prefix, rest = prefix+"[]", rest[2:]
		default:
			goto done
		}
	}
done:
	if !strings.Contains(rest, "/") {
		if i := strings.IndexByte(rest, '.'); i >= 0 {
			switch rest[:i] {
			case "time", "json", "sql":
			default:
				return "", fmt.Errorf(
					"type %q: unknown package %q; use a full import path "+
						"like \"github.com/you/pkg.Type\"", expr, rest[:i])
			}
		}
		return expr, nil
	}

	dot := strings.LastIndexByte(rest, '.')
	if dot < strings.LastIndexByte(rest, '/') {
		return "", fmt.Errorf("type %q: want \"import/path.Type\"", expr)
	}
	imp, typ := rest[:dot], rest[dot+1:]
	final := prefix + c.selectorFor(imp) + "." + typ
	c.typeImports[final] = imp
	return final, nil
}

// selectorFor returns the package selector to use for an import path, keeping
// selectors unique across imports. When two import paths share a base name
// (a/types and b/types), or the base is not a valid Go identifier (a path
// element with a dash), the selector is disambiguated and recorded as an
// explicit import alias so the generated code compiles regardless of the
// package's declared name.
func (c *compiler) selectorFor(imp string) string {
	if sel, ok := c.importSel[imp]; ok {
		return sel
	}
	natural := pkgName(imp)
	base := sanitizeSelector(natural)
	sel := base
	for n := 2; c.selUsed[sel] != "" && c.selUsed[sel] != imp; n++ {
		sel = base + strconv.Itoa(n)
	}
	c.selUsed[sel] = imp
	c.importSel[imp] = sel
	// An alias is needed whenever the selector is not exactly the natural base
	// (a collision suffix or a sanitized element), so the import binds to the
	// selector we actually reference.
	if sel != natural {
		c.importAlias[imp] = sel
	}
	return sel
}

// sanitizeSelector turns a guessed package base into a valid Go identifier,
// replacing every other rune with an underscore and prefixing p when it does
// not start with a letter or underscore.
func sanitizeSelector(base string) string {
	var b strings.Builder
	for _, r := range base {
		switch {
		case unicode.IsLetter(r) || r == '_':
			b.WriteRune(r)
		case unicode.IsDigit(r):
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	s := b.String()
	if s == "" {
		return "pkg"
	}
	if r, _ := utf8.DecodeRuneInString(s); !unicode.IsLetter(r) && r != '_' {
		return "p" + s
	}
	return s
}

// pkgName guesses the package name of an import path: the last element,
// skipping major-version suffixes (…/v5) and dropping gopkg.in-style
// versions (yaml.v3 is package yaml).
func pkgName(imp string) string {
	base := path.Base(imp)
	if len(base) > 1 && base[0] == 'v' && isDigits(base[1:]) {
		base = path.Base(path.Dir(imp))
	}
	if i := strings.IndexByte(base, '.'); i > 0 {
		base = base[:i]
	}
	return base
}

func isDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return len(s) > 0
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
