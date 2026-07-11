package gen

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// acronyms are the initialisms spelled in full caps inside generated
// identifiers, so user_api_id becomes UserAPIID, not UserApiId.
var acronyms = map[string]bool{
	"api": true, "css": true, "db": true, "dns": true, "html": true,
	"http": true, "https": true, "id": true, "ip": true, "json": true,
	"jwt": true, "sql": true, "ssl": true, "tls": true, "ttl": true,
	"uid": true, "uri": true, "url": true, "utf8": true, "uuid": true,
	"xml": true,
}

// CamelCase converts a snake_case database name to an exported Go
// identifier: user_api_id becomes UserAPIID. It always returns a valid,
// compilable exported identifier: non-identifier runes are dropped, the first
// rune is upper-cased on a rune boundary (so a non-ASCII name is not mangled),
// and a leading digit or an empty result is prefixed with X. PostgreSQL allows
// quoted names that are not valid Go identifiers, so this keeps generation from
// emitting code that will not compile.
func CamelCase(s string) string {
	var b strings.Builder
	for _, w := range splitWords(s) {
		w = keepIdentRunes(w)
		if w == "" {
			continue
		}
		if acronyms[w] {
			b.WriteString(strings.ToUpper(w))
			continue
		}
		b.WriteString(upperFirst(w))
	}
	return ensureExported(b.String())
}

// keepIdentRunes drops runes that cannot appear in a Go identifier (anything
// other than a Unicode letter, digit or underscore).
func keepIdentRunes(w string) string {
	var b strings.Builder
	for _, r := range w {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// upperFirst upper-cases the first rune of w on a rune boundary, so a non-ASCII
// name is not cut mid-rune.
func upperFirst(w string) string {
	if w == "" {
		return ""
	}
	r, size := utf8.DecodeRuneInString(w)
	return string(unicode.ToUpper(r)) + w[size:]
}

// ensureExported makes name a valid exported Go identifier: X for an empty
// name, and an X prefix when the first rune is not a letter (for example a
// column that begins with a digit).
func ensureExported(name string) string {
	if name == "" {
		return "X"
	}
	if r, _ := utf8.DecodeRuneInString(name); !unicode.IsLetter(r) {
		return "X" + name
	}
	return name
}

// snakeCase converts an exported Go identifier back to snake_case:
// RefreshRecord becomes refresh_record, Author becomes author. It is used for
// json tags on synthetic fields (embeds) that have no source column name, so
// the tag follows the Go field (its `as` alias) rather than the source table.
func snakeCase(s string) string {
	runes := []rune(s)
	var b strings.Builder
	for i, r := range runes {
		if r >= 'A' && r <= 'Z' {
			// A word boundary opens when the previous rune was lower/digit, or
			// when this upper rune ends an acronym run (the next rune is lower).
			if i > 0 {
				prev := runes[i-1]
				prevLowerOrDigit := (prev >= 'a' && prev <= 'z') || (prev >= '0' && prev <= '9')
				nextLower := i+1 < len(runes) && runes[i+1] >= 'a' && runes[i+1] <= 'z'
				if prevLowerOrDigit || nextLower {
					b.WriteByte('_')
				}
			}
			b.WriteRune(r - 'A' + 'a')
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// lowerCamel converts a snake_case name to an unexported identifier:
// user_id becomes userID, id stays id.
func lowerCamel(s string) string {
	var b strings.Builder
	first := true
	for _, w := range splitWords(s) {
		w = keepIdentRunes(w)
		if w == "" {
			continue
		}
		if first {
			b.WriteString(w) // first word stays lowercase
			first = false
			continue
		}
		if acronyms[w] {
			b.WriteString(strings.ToUpper(w))
			continue
		}
		b.WriteString(upperFirst(w))
	}
	return b.String()
}

// paramName turns a database-side name into a Go parameter name, keeping the
// result compilable when the name collides with a Go keyword.
func paramName(s string) string {
	name := lowerCamel(s)
	if name == "" {
		return "arg"
	}
	// An unexported identifier still cannot start with a digit; prefix x.
	if r, _ := utf8.DecodeRuneInString(name); !unicode.IsLetter(r) && r != '_' {
		name = "x" + name
	}
	if goKeywords[name] {
		return name + "_"
	}
	return name
}

// lowerFirst lowers the first letter of an already-camel identifier:
// GetUser becomes getUser.
func lowerFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToLower(s[:1]) + s[1:]
}

// splitWords cuts a database identifier into lowercase words.
func splitWords(s string) []string {
	var words []string
	for _, w := range strings.FieldsFunc(s, func(r rune) bool {
		return r == '_' || r == ' ' || r == '-' || r == '.'
	}) {
		words = append(words, strings.ToLower(w))
	}
	return words
}

var goKeywords = map[string]bool{
	"break": true, "case": true, "chan": true, "const": true,
	"continue": true, "default": true, "defer": true, "else": true,
	"fallthrough": true, "for": true, "func": true, "go": true,
	"goto": true, "if": true, "import": true, "interface": true,
	"map": true, "package": true, "range": true, "return": true,
	"select": true, "struct": true, "switch": true, "type": true,
	"var": true,
}
