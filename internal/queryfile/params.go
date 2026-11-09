package queryfile

import (
	"strconv"
	"strings"
)

// InferParamNames derives a human name for each $N parameter by scanning the
// SQL at the token level - no parsing, just the handful of patterns that
// cover real queries:
//
//   - column = $1 (any comparison or LIKE/ILIKE), in both directions;
//   - column IN ($1);
//   - INSERT INTO t (a, b) VALUES ($1, $2) - matched by position;
//   - LIMIT $1 and OFFSET $2.
//
// Names given explicitly with "-- param: $N name" (already in q.ParamNames)
// win; anything still unnamed falls back to argN. The result maps 1-based
// parameter numbers to raw (snake_case) names.
func InferParamNames(q Query, count int) map[int]string {
	names := map[int]string{}
	toks := tokenize(q.SQL)

	insertValues(toks, names)

	for i, t := range toks {
		if t.kind != tokParam {
			continue
		}
		if _, ok := names[t.num]; ok {
			continue
		}
		if name := neighborName(toks, i); name != "" {
			names[t.num] = name
		}
	}

	// Explicit annotations win; argN fills the gaps.
	for n := 1; n <= count; n++ {
		if name, ok := q.ParamNames[n]; ok {
			names[n] = name
		} else if _, ok := names[n]; !ok {
			names[n] = "arg" + strconv.Itoa(n)
		}
	}
	return names
}

// neighborName looks around one $N token for a column it is compared with.
func neighborName(toks []token, i int) string {
	// LIMIT $1 / OFFSET $1.
	if prev := at(toks, i-1); isWord(prev, "limit") || isWord(prev, "offset") {
		return prev.val
	}
	// column op $1 (op includes LIKE/ILIKE keywords).
	if isCompare(at(toks, i-1)) {
		if id := at(toks, i-2); isColumn(id) {
			return id.val
		}
	}
	// $1 op column, where column may be qualified: $1 = p."order".
	if isCompare(at(toks, i+1)) {
		if id := chainEnd(toks, i+2); isColumn(id) {
			return id.val
		}
	}
	// column IN ($1).
	if isPunct(at(toks, i-1), "(") && isWord(at(toks, i-2), "in") {
		if id := at(toks, i-3); isColumn(id) {
			return id.val
		}
	}
	// column = ANY($1) / ALL($1).
	if isPunct(at(toks, i-1), "(") &&
		(isWord(at(toks, i-2), "any") || isWord(at(toks, i-2), "all")) &&
		isCompare(at(toks, i-3)) {
		if id := at(toks, i-4); isColumn(id) {
			return id.val
		}
	}
	// $1 = ANY(column) / ALL(column).
	if isCompare(at(toks, i+1)) &&
		(isWord(at(toks, i+2), "any") || isWord(at(toks, i+2), "all")) &&
		isPunct(at(toks, i+3), "(") {
		if id := chainEnd(toks, i+4); isColumn(id) {
			return id.val
		}
	}
	return ""
}

// chainEnd returns the last identifier of a qualified name starting at i
// (t.col, s.t.col), or the token at i itself when no chain starts there. The
// tokens before a parameter already end in the column; after it the scan
// starts at the qualifier and must walk to the column.
func chainEnd(toks []token, i int) token {
	t := at(toks, i)
	for t.kind == tokIdent && isPunct(at(toks, i+1), ".") && at(toks, i+2).kind == tokIdent {
		i += 2
		t = toks[i]
	}
	return t
}

// insertValues matches INSERT INTO t (c1, c2, ...) VALUES (...) and names
// the parameters of the first tuple by column position.
func insertValues(toks []token, names map[int]string) {
	i := 0
	for ; i < len(toks); i++ {
		if isWord(toks[i], "insert") && isWord(at(toks, i+1), "into") {
			break
		}
	}
	if i == len(toks) {
		return
	}

	// Skip the (possibly qualified) table name to the column list.
	i += 2
	for i < len(toks) && (toks[i].kind == tokIdent || isPunct(toks[i], ".")) {
		i++
	}
	if !isPunct(at(toks, i), "(") {
		return
	}

	var cols []string
	for i++; i < len(toks) && !isPunct(toks[i], ")"); i++ {
		if toks[i].kind == tokIdent {
			cols = append(cols, toks[i].val)
		}
	}

	for ; i < len(toks); i++ {
		if isWord(toks[i], "values") {
			break
		}
	}
	if i == len(toks) || !isPunct(at(toks, i+1), "(") {
		return
	}

	// Walk the first VALUES tuple; a position that is exactly one $N gets
	// the column name at the same index.
	depth, arg := 1, 0
	var argToks []token
	commit := func() {
		if len(argToks) == 1 && argToks[0].kind == tokParam && arg < len(cols) {
			if _, ok := names[argToks[0].num]; !ok {
				names[argToks[0].num] = cols[arg]
			}
		}
		argToks = nil
	}
	for i += 2; i < len(toks) && depth > 0; i++ {
		switch {
		case isPunct(toks[i], "("):
			depth++
		case isPunct(toks[i], ")"):
			depth--
			if depth == 0 {
				commit()
				return
			}
		case isPunct(toks[i], ","):
			if depth == 1 {
				commit()
				arg++
				continue
			}
		}
		if depth >= 1 && !isPunct(toks[i], ")") {
			argToks = append(argToks, toks[i])
		}
	}
}

// --- tiny SQL tokenizer -----------------------------------------------------

type tokKind int

const (
	tokIdent tokKind = iota // identifier or keyword, val lowercased
	tokParam                // $N, num holds N
	tokOp                   // run of operator characters
	tokPunct                // single punctuation: ( ) , . ;
	tokOther                // literals and anything else, skipped over
)

type token struct {
	kind   tokKind
	val    string
	num    int
	quoted bool // a "quoted identifier": spelled exactly, never a keyword
}

var empty = token{kind: tokOther}

func at(toks []token, i int) token {
	if i < 0 || i >= len(toks) {
		return empty
	}
	return toks[i]
}

// isWord reports whether t is the unquoted keyword w. A quoted identifier
// spelled the same ("values", "in") is a column or table, not the keyword.
func isWord(t token, w string) bool {
	return t.kind == tokIdent && !t.quoted && t.val == w
}

// isPunct reports whether t is the punctuation p.
func isPunct(t token, p string) bool {
	return t.kind == tokPunct && t.val == p
}

// isColumn reports whether t can name a column next to a parameter: any
// quoted identifier, or an unquoted one that is not a keyword. The quotes are
// exactly what makes "order" or "group" a column in PostgreSQL, so they must
// not be filtered as keywords.
func isColumn(t token) bool {
	return t.kind == tokIdent && (t.quoted || !isKeyword(t.val))
}

func isCompare(t token) bool {
	if t.kind == tokIdent {
		return !t.quoted && (t.val == "like" || t.val == "ilike")
	}
	if t.kind != tokOp {
		return false
	}
	switch t.val {
	case "=", "<", ">", "<=", ">=", "<>", "!=":
		return true
	}
	return false
}

// isKeyword filters the unquoted words that must never become a parameter
// name when they appear next to an operator.
func isKeyword(s string) bool {
	switch s {
	case "select", "from", "where", "and", "or", "not", "null", "is",
		"insert", "into", "values", "update", "set", "delete", "returning",
		"order", "by", "group", "having", "limit", "offset", "join", "on",
		"left", "right", "full", "inner", "outer", "as", "in", "between",
		"true", "false", "case", "when", "then", "else", "end", "asc", "desc",
		"any", "all", "some", "exists", "distinct":
		return true
	}
	return false
}

// tokenize splits SQL into the tokens the heuristics need, skipping string
// literals, quoted identifiers and comments.
func tokenize(sql string) []token {
	var toks []token
	s := sql
	for len(s) > 0 {
		c := s[0]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			s = s[1:]

		case strings.HasPrefix(s, "--"):
			if i := strings.IndexByte(s, '\n'); i >= 0 {
				s = s[i+1:]
			} else {
				s = ""
			}

		case strings.HasPrefix(s, "/*"):
			if i := strings.Index(s, "*/"); i >= 0 {
				s = s[i+2:]
			} else {
				s = ""
			}

		case c == '\'':
			s = skipQuoted(s, '\'')
			toks = append(toks, token{kind: tokOther})

		case c == '"':
			// A quoted identifier keeps its exact spelling and is never a
			// keyword, however it is spelled.
			var val string
			val, s = readQuoted(s)
			toks = append(toks, token{kind: tokIdent, val: val, quoted: true})

		case c == '$':
			n, rest, ok := readParam(s)
			if ok {
				toks = append(toks, token{kind: tokParam, num: n})
				s = rest
			} else {
				// $tag$ ... $tag$ dollar-quoted literal.
				s = skipDollarQuoted(s)
				toks = append(toks, token{kind: tokOther})
			}

		case isIdentStart(c):
			j := 1
			for j < len(s) && isIdentPart(s[j]) {
				j++
			}
			toks = append(toks, token{kind: tokIdent, val: strings.ToLower(s[:j])})
			s = s[j:]

		case c >= '0' && c <= '9':
			j := 1
			for j < len(s) && (s[j] >= '0' && s[j] <= '9' || s[j] == '.') {
				j++
			}
			toks = append(toks, token{kind: tokOther})
			s = s[j:]

		case isOpChar(c):
			j := 1
			for j < len(s) && isOpChar(s[j]) {
				j++
			}
			toks = append(toks, token{kind: tokOp, val: s[:j]})
			s = s[j:]

		default:
			toks = append(toks, token{kind: tokPunct, val: string(c)})
			s = s[1:]
		}
	}
	return toks
}

func isIdentStart(c byte) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

func isIdentPart(c byte) bool {
	return isIdentStart(c) || c >= '0' && c <= '9'
}

func isOpChar(c byte) bool {
	switch c {
	case '=', '<', '>', '!', '~', '+', '-', '*', '/', '%', '^', '|', '&', '#', '@':
		return true
	}
	return false
}

// readParam reads $N; ok is false when $ starts a dollar-quote instead.
func readParam(s string) (int, string, bool) {
	j := 1
	for j < len(s) && s[j] >= '0' && s[j] <= '9' {
		j++
	}
	if j == 1 {
		return 0, s, false
	}
	n, _ := strconv.Atoi(s[1:j])
	return n, s[j:], true
}

// skipQuoted skips a 'literal' with doubled-quote escapes.
func skipQuoted(s string, q byte) string {
	for i := 1; i < len(s); i++ {
		if s[i] == q {
			if i+1 < len(s) && s[i+1] == q {
				i++
				continue
			}
			return s[i+1:]
		}
	}
	return ""
}

// readQuoted reads a "quoted identifier" and returns its unescaped content and
// the rest of the input. A doubled quote inside the identifier is an escaped
// literal quote ("user""id" is the identifier user"id), so the closing quote is
// only the first quote not immediately followed by another.
func readQuoted(s string) (string, string) {
	var b strings.Builder
	for i := 1; i < len(s); i++ {
		if s[i] == '"' {
			if i+1 < len(s) && s[i+1] == '"' {
				b.WriteByte('"')
				i++
				continue
			}
			return b.String(), s[i+1:]
		}
		b.WriteByte(s[i])
	}
	return b.String(), ""
}

// skipDollarQuoted skips $tag$ ... $tag$ literals.
func skipDollarQuoted(s string) string {
	end := strings.IndexByte(s[1:], '$')
	if end < 0 {
		return ""
	}
	tag := s[:end+2] // "$tag$"
	rest := s[len(tag):]
	if i := strings.Index(rest, tag); i >= 0 {
		return rest[i+len(tag):]
	}
	return ""
}
