package compile

import (
	"strings"

	"github.com/goloop/pgc/internal/queryfile"
)

// neverNull reports whether a result expression cannot produce NULL, whatever
// the data is. The catalog cannot answer this: an expression has no
// pg_attribute row, so every one of them would otherwise be rendered as a
// pointer and unwrapped by hand at the call site.
//
// The list is short on purpose. A rule that is right most of the time is worse
// than no rule: an override is a decision the author made, while a wrong
// inference is a NULL arriving in a value that cannot hold one, at run time,
// on a row nobody tested with. Anything not spelled out here stays nullable
// and can be corrected with "-- override: <column> notnull".
//
//   - count(...) - defined to return 0 for no rows, never NULL.
//   - coalesce(..., <literal>) - a non-null literal last argument is what
//     makes coalesce total; coalesce(a, b) of two columns is not.
//   - a non-null literal, with or without a cast: 'x', 0, true, 'x'::text.
func neverNull(expr string) bool {
	e := strings.TrimSpace(stripAlias(expr))

	if args, ok := callArgs(e, "count"); ok {
		_ = args
		return true
	}
	if args, ok := callArgs(e, "coalesce"); ok {
		return len(args) > 0 && isLiteral(args[len(args)-1])
	}
	return isLiteral(e)
}

// callArgs reports whether e is exactly a call of name and returns its
// arguments. "Exactly" matters: count(x) + 1 is not count(x), and
// f(coalesce(a, 'b')) is not a coalesce.
func callArgs(e, name string) ([]string, bool) {
	if len(e) <= len(name)+1 || !strings.EqualFold(e[:len(name)], name) {
		return nil, false
	}
	rest := strings.TrimSpace(e[len(name):])
	if !strings.HasPrefix(rest, "(") || !strings.HasSuffix(rest, ")") {
		return nil, false
	}

	// The closing parenthesis must be the one this call opened, or the
	// expression is something larger that merely ends here - count(*) + 1
	// is not a count. Parentheses inside a string do not nest.
	depth := 0
	var quote byte
	for i := 0; i < len(rest); i++ {
		c := rest[i]
		if quote != 0 {
			if c == quote {
				quote = 0
			}
			continue
		}
		switch c {
		case '\'', '"':
			quote = c
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 && i != len(rest)-1 {
				return nil, false
			}
		}
	}
	if depth != 0 || quote != 0 {
		return nil, false
	}

	inner := strings.TrimSpace(rest[1 : len(rest)-1])
	if inner == "" {
		return nil, false
	}
	return splitArgs(inner), true
}

// splitArgs cuts a call's arguments on top-level commas.
func splitArgs(s string) []string {
	var (
		args  []string
		depth int
		start int
		quote byte
	)
	for i := 0; i < len(s); i++ {
		c := s[i]
		if quote != 0 {
			if c == quote {
				quote = 0
			}
			continue
		}
		switch c {
		case '\'', '"':
			quote = c
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				args = append(args, s[start:i])
				start = i + 1
			}
		}
	}
	return append(args, s[start:])
}

// isLiteral reports whether e is a non-null constant, optionally cast to a
// built-in type. NULL itself is not one, nor is anything that reads a column.
//
// The cast target has to be built in. A cast to a user-defined type runs a
// user-defined function, and one declared non-strict may return NULL for a
// perfectly non-null input - so 'x'::text is a constant while 1::my_type is
// only an expression that starts with one.
func isLiteral(e string) bool {
	parts := strings.Split(strings.TrimSpace(e), "::")
	for _, cast := range parts[1:] {
		if !builtinType(cast) {
			return false
		}
	}

	e = strings.TrimSpace(parts[0])
	if e == "" {
		return false
	}

	if e[0] == '\'' {
		// A quoted string, and nothing after the quote that closes it.
		return strings.HasSuffix(e, "'") && !strings.Contains(e[1:len(e)-1], "'")
	}
	switch strings.ToLower(e) {
	case "true", "false":
		return true
	case "null":
		return false
	}
	for i := 0; i < len(e); i++ {
		if (e[i] < '0' || e[i] > '9') && e[i] != '.' && !(i == 0 && (e[i] == '-' || e[i] == '+')) {
			return false
		}
	}
	return true
}

// builtinTypes are the cast targets a constant may wear and stay a constant.
// The list only ever narrows what is inferred, so a type missing from it costs
// a pointer somebody can remove with an override - never a NULL in a value
// that cannot hold one.
var builtinTypes = map[string]bool{
	"bigint": true, "bool": true, "boolean": true, "bpchar": true,
	"bytea": true, "char": true, "character": true, "date": true,
	"decimal": true, "double": true, "float4": true, "float8": true,
	"inet": true, "int": true, "int2": true, "int4": true, "int8": true,
	"integer": true, "interval": true, "json": true, "jsonb": true,
	"numeric": true, "real": true, "smallint": true, "text": true,
	"time": true, "timestamp": true, "timestamptz": true, "timetz": true,
	"uuid": true, "varchar": true,
}

// builtinType reports whether a cast target names a built-in type, ignoring a
// length or precision, an array suffix and any schema qualification of
// pg_catalog.
func builtinType(cast string) bool {
	name := strings.TrimSpace(cast)
	if i := strings.IndexByte(name, '('); i >= 0 {
		if !strings.HasSuffix(name, ")") {
			return false
		}
		name = name[:i]
	}
	name = strings.TrimSpace(strings.TrimSuffix(name, "[]"))
	name = strings.TrimPrefix(strings.ToLower(name), "pg_catalog.")

	// "double precision", "timestamp with time zone" and friends: the first
	// word carries the identity, the rest is grammar.
	if i := strings.IndexAny(name, " \t\n"); i >= 0 {
		name = name[:i]
	}
	return builtinTypes[name]
}

// stripAlias removes the output name from a select-list item, so the
// expression can be looked at on its own.
func stripAlias(e string) string {
	e = strings.TrimSpace(e)
	if i, ok := aliasKeyword(e); ok {
		return e[:i]
	}

	// A bare alias, as in "count(*) total". It is only an alias when what
	// precedes it ends an expression; in "a + b" the trailing b is the
	// expression.
	j := len(e)
	for j > 0 && isWordByte(e[j-1]) {
		j--
	}
	if j == 0 || j == len(e) {
		return e
	}
	k := j
	for k > 0 && (e[k-1] == ' ' || e[k-1] == '\t' || e[k-1] == '\n') {
		k--
	}
	if k == j || k == 0 {
		return e
	}
	if prev := e[k-1]; prev == ')' || prev == '\'' || prev >= '0' && prev <= '9' {
		return e[:k]
	}
	return e
}

// aliasKeyword finds a top-level " AS " and returns where the expression ends.
func aliasKeyword(e string) (int, bool) {
	depth := 0
	for i := 0; i < len(e); i++ {
		switch e[i] {
		case '(':
			depth++
		case ')':
			depth--
		}
		if depth != 0 || i == 0 || !isSpaceByte(e[i-1]) {
			continue
		}
		if i+2 <= len(e) && strings.EqualFold(e[i:i+2], "as") &&
			(i+2 == len(e) || isSpaceByte(e[i+2])) {
			return i - 1, true
		}
	}
	return 0, false
}

func isWordByte(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' ||
		b >= '0' && b <= '9' || b == '_'
}

func isSpaceByte(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r'
}

// expressionNotNull reports whether the result column at index i is an
// expression this package can prove never returns NULL.
//
// It needs the whole output list to line up with the described columns; when
// SelectList cannot promise that, nothing is inferred at all rather than
// inferred for the wrong column.
func expressionNotNull(sql string, index, columns int) bool {
	items, ok := queryfile.SelectList(sql)
	if !ok || len(items) != columns || index >= len(items) {
		return false
	}
	return neverNull(items[index])
}
