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
			i = skipQuoted(sql, i, '\'')
		case c == '"':
			i = skipQuoted(sql, i, '"')
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

// skipQuoted advances past a quoted region opened at i, where a doubled
// quote is an escape.
func skipQuoted(sql string, i int, q byte) int {
	for i++; i < len(sql); i++ {
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
