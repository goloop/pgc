package migrate

import "strings"

// splitStatements cuts a SQL script into its top-level statements, honoring
// string literals, quoted identifiers, dollar-quoted bodies and both comment
// forms, so a semicolon inside any of them never splits. Empty statements
// are dropped.
func splitStatements(sql string) []string {
	var stmts []string
	start := 0
	i := 0

	flush := func(end int) {
		s := strings.TrimSpace(sql[start:end])
		if s != "" {
			stmts = append(stmts, s)
		}
	}

	for i < len(sql) {
		c := sql[i]
		switch {
		case c == '\'':
			i = skipQuoted(sql, i, '\'', isEString(sql, i))
		case c == '"':
			i = skipQuoted(sql, i, '"', false)
		case strings.HasPrefix(sql[i:], "--"):
			if j := strings.IndexByte(sql[i:], '\n'); j >= 0 {
				i += j + 1
			} else {
				i = len(sql)
			}
		case strings.HasPrefix(sql[i:], "/*"):
			i = skipBlockComment(sql, i)
		case c == '$':
			i = skipDollarQuoted(sql, i)
		case c == ';':
			flush(i)
			i++
			start = i
		default:
			i++
		}
	}
	flush(len(sql))
	return stmts
}

// skipQuoted advances past a quoted region opened at i, where a doubled quote
// is an escape. When backslashEscapes is set (a PostgreSQL E'...' escape
// string), a backslash also escapes the next byte, so E'a\';b' is one literal
// and its embedded semicolon does not split the statement.
func skipQuoted(sql string, i int, q byte, backslashEscapes bool) int {
	for i++; i < len(sql); i++ {
		if backslashEscapes && sql[i] == '\\' {
			i++ // the loop's i++ skips the escaped byte as well
			continue
		}
		if sql[i] == q {
			if i+1 < len(sql) && sql[i+1] == q {
				i++
				continue
			}
			return i + 1
		}
	}
	return len(sql)
}

// isEString reports whether the single quote at i opens a PostgreSQL escape
// string, i.e. it is immediately preceded by a standalone E (or e) token.
func isEString(sql string, i int) bool {
	if i == 0 {
		return false
	}
	if p := sql[i-1]; p != 'E' && p != 'e' {
		return false
	}
	// The E must be a standalone prefix, not the tail of an identifier such as
	// the "e" in "true".
	if i-1 == 0 {
		return true
	}
	return !isIdentByte(sql[i-2])
}

// isIdentByte reports whether c can appear inside a SQL identifier.
func isIdentByte(c byte) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' ||
		c >= '0' && c <= '9'
}

// skipBlockComment advances past a /* */ comment opened at i; PostgreSQL
// block comments nest.
func skipBlockComment(sql string, i int) int {
	depth := 0
	for i < len(sql) {
		switch {
		case strings.HasPrefix(sql[i:], "/*"):
			depth++
			i += 2
		case strings.HasPrefix(sql[i:], "*/"):
			depth--
			i += 2
			if depth == 0 {
				return i
			}
		default:
			i++
		}
	}
	return len(sql)
}

// skipDollarQuoted advances past a $tag$ ... $tag$ body opened at i. When
// the $ does not open a valid dollar-quote tag, it is a plain character.
func skipDollarQuoted(sql string, i int) int {
	end := i + 1
	for end < len(sql) && (isTagChar(sql[end]) || sql[end] == '$') {
		if sql[end] == '$' {
			tag := sql[i : end+1]
			if j := strings.Index(sql[end+1:], tag); j >= 0 {
				return end + 1 + j + len(tag)
			}
			return len(sql)
		}
		end++
	}
	return i + 1
}

func isTagChar(c byte) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' ||
		c >= '0' && c <= '9'
}
