package gen

import "strings"

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
// identifier: user_api_id becomes UserAPIID.
func CamelCase(s string) string {
	var b strings.Builder
	for _, w := range splitWords(s) {
		if acronyms[w] {
			b.WriteString(strings.ToUpper(w))
			continue
		}
		b.WriteString(strings.ToUpper(w[:1]))
		b.WriteString(w[1:])
	}
	return b.String()
}

// lowerCamel converts a snake_case name to an unexported identifier:
// user_id becomes userID, id stays id.
func lowerCamel(s string) string {
	words := splitWords(s)
	if len(words) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(words[0])
	for _, w := range words[1:] {
		if acronyms[w] {
			b.WriteString(strings.ToUpper(w))
			continue
		}
		b.WriteString(strings.ToUpper(w[:1]))
		b.WriteString(w[1:])
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
