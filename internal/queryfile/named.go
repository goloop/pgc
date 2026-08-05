package queryfile

import (
	"fmt"
	"strings"
)

// rewriteNamed turns @name placeholders into the $N the server understands,
// numbering them by first appearance. A repeated @name reuses its number, so
// the same value is sent once and referred to twice.
//
//	INSERT INTO t (a, b) VALUES (@title, @slug)  ->  VALUES ($1, $2)
//	WHERE lower(a) = @q OR lower(b) = @q         ->  = $1 OR ... = $1
//
// It returns the rewritten SQL and the names in parameter order, or an empty
// slice when the query has no named placeholders.
//
// The scan is a lexer, not a parser: it only needs to know where SQL text is
// literal. An "@" inside a string, a quoted identifier or a comment is left
// alone, and so is an operator - "@>", "<@" and "@@" are not followed by a
// letter, which is what makes a placeholder.
func rewriteNamed(sql string) (string, []string, error) {
	var (
		b        strings.Builder
		names    []string
		numberOf = map[string]int{}
		mixed    bool
	)
	b.Grow(len(sql))

	for i := 0; i < len(sql); {
		if skip := literalRun(sql, i); skip > i {
			b.WriteString(sql[i:skip])
			i = skip
			continue
		}

		c := sql[i]
		if c == '$' && i+1 < len(sql) && isDigit(sql[i+1]) {
			mixed = true
			b.WriteByte(c)
			i++
			continue
		}
		if c != '@' || i+1 >= len(sql) || !isNameStart(sql[i+1]) {
			b.WriteByte(c)
			i++
			continue
		}

		j := i + 1
		for j < len(sql) && isNameChar(sql[j]) {
			j++
		}
		name := sql[i+1 : j]

		n, seen := numberOf[name]
		if !seen {
			names = append(names, name)
			n = len(names)
			numberOf[name] = n
		}
		fmt.Fprintf(&b, "$%d", n)
		i = j
	}

	if len(names) > 0 && mixed {
		return "", nil, fmt.Errorf(
			"query mixes named parameters (@%s) with positional ones ($N); "+
				"use one style or the other", names[0])
	}
	if len(names) == 0 {
		return sql, nil, nil
	}
	return b.String(), names, nil
}

// literalRun reports the end of the stretch of SQL starting at i that must be
// copied through untouched - a string, a quoted identifier, a dollar-quoted
// body or a comment. It returns i itself when the position starts none of
// those, so the caller can look at the byte.
func literalRun(sql string, i int) int {
	switch sql[i] {
	case '\'':
		return quoted(sql, i, '\'', escaping(sql, i))
	case '"':
		return quoted(sql, i, '"', false)
	case '-':
		if strings.HasPrefix(sql[i:], "--") {
			if nl := strings.IndexByte(sql[i:], '\n'); nl >= 0 {
				return i + nl // leave the newline to the main loop
			}
			return len(sql)
		}
	case '/':
		if strings.HasPrefix(sql[i:], "/*") {
			return blockComment(sql, i)
		}
	case '$':
		if end, ok := dollarQuoted(sql, i); ok {
			return end
		}
	}
	return i
}

// quoted returns the end of a quoted run opened at i with q. A doubled quote
// is an escaped quote and does not end the run; with backslash escaping on
// (an E” string), a backslash also protects the next byte. An unterminated
// run reaches the end of the input, which the server will report far better
// than this scanner could.
func quoted(sql string, i int, q byte, backslash bool) int {
	for j := i + 1; j < len(sql); j++ {
		switch sql[j] {
		case '\\':
			if backslash {
				j++
			}
		case q:
			if j+1 < len(sql) && sql[j+1] == q {
				j++
				continue
			}
			return j + 1
		}
	}
	return len(sql)
}

// escaping reports whether the string opening at i is an E” escape string,
// where a backslash escapes the next byte. The E must be a token of its own,
// not the tail of an identifier such as "value".
func escaping(sql string, i int) bool {
	if i == 0 || (sql[i-1] != 'E' && sql[i-1] != 'e') {
		return false
	}
	return i == 1 || !isNameChar(sql[i-2])
}

// blockComment returns the end of a /* */ comment opened at i. They nest in
// PostgreSQL, so the scan counts depth rather than stopping at the first
// closing pair.
func blockComment(sql string, i int) int {
	depth := 0
	for j := i; j < len(sql)-1; j++ {
		switch {
		case sql[j] == '/' && sql[j+1] == '*':
			depth++
			j++
		case sql[j] == '*' && sql[j+1] == '/':
			depth--
			j++
			if depth == 0 {
				return j + 1
			}
		}
	}
	return len(sql)
}

// dollarQuoted returns the end of a $tag$...$tag$ body opened at i, and
// whether i opens one at all. A "$" followed by a digit is a parameter rather
// than a tag, and an opening tag with no closing one is not treated as a body
// running to the end of the input: an identifier may contain "$", so a lone
// "price$usd$total" would otherwise swallow the rest of the statement. An
// unbalanced tag is invalid SQL, and the server says so.
func dollarQuoted(sql string, i int) (int, bool) {
	j := i + 1
	for j < len(sql) && sql[j] != '$' {
		if !isNameChar(sql[j]) || j == i+1 && isDigit(sql[j]) {
			return 0, false
		}
		j++
	}
	if j >= len(sql) {
		return 0, false
	}

	tag := sql[i : j+1]
	end := strings.Index(sql[j+1:], tag)
	if end < 0 {
		return 0, false
	}
	return j + 1 + end + len(tag), true
}

// isNameStart reports whether b can begin a parameter name.
func isNameStart(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b == '_'
}

// isNameChar reports whether b can continue a parameter name.
func isNameChar(b byte) bool {
	return isNameStart(b) || isDigit(b)
}

// isDigit reports whether b is an ASCII digit.
func isDigit(b byte) bool { return b >= '0' && b <= '9' }
