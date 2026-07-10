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
	if prev := at(toks, i-1); prev.kind == tokIdent {
		switch prev.val {
		case "limit", "offset":
			return prev.val
		}
	}
	// column op $1 (op includes LIKE/ILIKE keywords).
	if isCompare(at(toks, i-1)) {
		if id := at(toks, i-2); id.kind == tokIdent && !isKeyword(id.val) {
			return id.val
		}
	}
	// $1 op column.
	if isCompare(at(toks, i+1)) {
		if id := at(toks, i+2); id.kind == tokIdent && !isKeyword(id.val) {
			return id.val
		}
	}
	// column IN ($1).
	if at(toks, i-1).val == "(" && at(toks, i-2).val == "in" {
		if id := at(toks, i-3); id.kind == tokIdent && !isKeyword(id.val) {
			return id.val
		}
	}
	return ""
}

// insertValues matches INSERT INTO t (c1, c2, ...) VALUES (...) and names
// the parameters of the first tuple by column position.
func insertValues(toks []token, names map[int]string) {
	i := 0
	for ; i < len(toks); i++ {
		if toks[i].val == "insert" && at(toks, i+1).val == "into" {
			break
		}
	}
	if i == len(toks) {
		return
	}

	// Skip the (possibly qualified) table name to the column list.
	i += 2
	for i < len(toks) && (toks[i].kind == tokIdent || toks[i].val == ".") {
		i++
	}
	if at(toks, i).val != "(" {
		return
	}

	var cols []string
	for i++; i < len(toks) && toks[i].val != ")"; i++ {
		if toks[i].kind == tokIdent {
			cols = append(cols, toks[i].val)
		}
	}

	for ; i < len(toks); i++ {
		if toks[i].val == "values" {
			break
		}
	}
	if i == len(toks) || at(toks, i+1).val != "(" {
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
		switch toks[i].val {
		case "(":
			depth++
		case ")":
			depth--
			if depth == 0 {
				commit()
				return
			}
		case ",":
			if depth == 1 {
				commit()
				arg++
				continue
			}
		}
		if depth >= 1 && toks[i].val != ")" {
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
	kind tokKind
	val  string
	num  int
}

var empty = token{kind: tokOther}

func at(toks []token, i int) token {
	if i < 0 || i >= len(toks) {
		return empty
	}
	return toks[i]
}

func isCompare(t token) bool {
	if t.kind == tokIdent {
		return t.val == "like" || t.val == "ilike"
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

// isKeyword filters the words that must never become a parameter name when
// they appear next to an operator.
func isKeyword(s string) bool {
	switch s {
	case "select", "from", "where", "and", "or", "not", "null", "is",
		"insert", "into", "values", "update", "set", "delete", "returning",
		"order", "by", "group", "having", "limit", "offset", "join", "on",
		"left", "right", "full", "inner", "outer", "as", "in", "between",
		"true", "false", "case", "when", "then", "else", "end", "asc", "desc":
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
			// A quoted identifier keeps its exact spelling.
			var val string
			val, s = readQuoted(s)
			toks = append(toks, token{kind: tokIdent, val: val})

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

// readQuoted reads a "quoted identifier" and returns its content.
func readQuoted(s string) (string, string) {
	for i := 1; i < len(s); i++ {
		if s[i] == '"' {
			return s[1:i], s[i+1:]
		}
	}
	return s[1:], ""
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
