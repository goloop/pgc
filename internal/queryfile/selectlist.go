package queryfile

import "strings"

// SelectList splits the output list of a statement - the expressions between
// SELECT and FROM, or the ones after RETURNING - into one string per result
// column, in order. The second result reports whether the split can be
// trusted to line up with the columns the server describes.
//
// It is deliberately quick to give up. Everything downstream of it decides
// what a column's Go type is, and a list that lines up by luck would put the
// wrong decision on the wrong column. A wildcard, a set operation, a CTE, a
// DISTINCT ON - anything where position stops being obvious - returns false,
// and the caller falls back to what the catalog alone can prove.
func SelectList(sql string) ([]string, bool) {
	start, ok := listStart(sql)
	if !ok {
		return nil, false
	}

	list, ok := listEnd(sql, start)
	if !ok {
		return nil, false
	}

	items := splitTopLevel(list)
	for _, it := range items {
		it = strings.TrimSpace(it)
		if it == "" || it == "*" || strings.HasSuffix(it, ".*") {
			return nil, false
		}
	}
	if len(items) == 0 {
		return nil, false
	}
	return items, true
}

// listStart returns the offset just past the keyword that opens the output
// list. A plain SELECT opens its own; INSERT, UPDATE and DELETE open theirs
// with RETURNING. A leading WITH is refused: the outer statement's list is
// somewhere past a run of CTEs, and finding it is parsing.
func listStart(sql string) (int, bool) {
	first, at := firstWord(sql)
	switch first {
	case "select":
		i := at + len("select")
		// DISTINCT is transparent, but DISTINCT ON (...) puts an expression
		// in front of the list that answers to no column.
		if w, wat := firstWord(sql[i:]); w == "distinct" {
			i += wat + len("distinct")
			if w, _ := firstWord(sql[i:]); w == "on" {
				return 0, false
			}
		}
		return i, true
	case "insert", "update", "delete":
		if i, ok := topLevelWord(sql, "returning"); ok {
			return i + len("returning"), true
		}
	}
	return 0, false
}

// listEnd returns the output list that begins at start. It stops at the
// top-level FROM, or at the end of the statement for a list that has none.
func listEnd(sql string, start int) (string, bool) {
	// A set operation makes the described columns those of the combined
	// result, which no single list accounts for.
	for _, op := range []string{"union", "intersect", "except"} {
		if _, found := topLevelWord(sql[start:], op); found {
			return "", false
		}
	}
	if i, ok := topLevelWord(sql[start:], "from"); ok {
		return sql[start : start+i], true
	}
	return sql[start:], true
}

// firstWord returns the first bare word of s, lowercased, and its offset.
// Leading whitespace and comments are skipped; a word inside a literal is not
// a first word, since a statement cannot begin inside one.
func firstWord(s string) (string, int) {
	for i := 0; i < len(s); {
		if skip := literalRun(s, i); skip > i {
			i = skip
			continue
		}
		if !isNameStart(s[i]) {
			i++
			continue
		}
		j := i
		for j < len(s) && isNameChar(s[j]) {
			j++
		}
		return strings.ToLower(s[i:j]), i
	}
	return "", 0
}

// topLevelWord finds word as a bare token outside any parentheses, string or
// comment, and returns its offset.
func topLevelWord(s, word string) (int, bool) {
	depth := 0
	for i := 0; i < len(s); {
		if skip := literalRun(s, i); skip > i {
			i = skip
			continue
		}
		switch {
		case s[i] == '(':
			depth++
			i++
		case s[i] == ')':
			depth--
			i++
		case isNameStart(s[i]):
			j := i
			for j < len(s) && isNameChar(s[j]) {
				j++
			}
			if depth == 0 && strings.EqualFold(s[i:j], word) {
				return i, true
			}
			i = j
		default:
			i++
		}
	}
	return 0, false
}

// splitTopLevel cuts a list on the commas that are not inside parentheses, a
// string, an identifier or a comment.
func splitTopLevel(s string) []string {
	var (
		items []string
		depth int
		start int
	)
	for i := 0; i < len(s); {
		if skip := literalRun(s, i); skip > i {
			i = skip
			continue
		}
		switch s[i] {
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				items = append(items, s[start:i])
				start = i + 1
			}
		}
		i++
	}
	if rest := strings.TrimSpace(s[start:]); rest != "" || len(items) > 0 {
		items = append(items, s[start:])
	}
	return items
}
