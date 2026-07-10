// Package queryfile reads the annotated .sql files that feed pgc: it splits
// them into named queries, collects their documentation comments and decodes
// the per-query annotations (overrides and parameter names). It does not
// parse SQL - the query body is passed to the server verbatim.
package queryfile

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Query is one named query from a .sql file, still untyped: the type
// information comes later from the database.
type Query struct {
	Name    string // exported Go name from the annotation, e.g. GetUser
	Command string // one, many, exec or execrows
	Doc     string // annotation comment lines joined with a space
	SQL     string // the statement body, verbatim, without a trailing ;

	File string // source path, for error messages and headers
	Line int    // 1-based line of the -- name: header

	Overrides  []Override
	ParamNames map[int]string // $N (1-based) to a name from -- param:
	Embeds     []Embed        // "-- embed:" annotations, in order
}

// Embed asks for a table's full column run inside the result to be nested
// as that table's model struct, from a "-- embed: <table> [as <Field>]"
// annotation.
type Embed struct {
	Table string // table name, optionally schema-qualified
	As    string // Go field name; empty means the model name
}

// Override adjusts the Go type or nullability of one result column or one
// $N parameter, from a "-- override:" annotation.
type Override struct {
	Column string // result column name; empty when Param is set
	Param  int    // $N parameter number; 0 when Column is set
	GoType string // replacement Go type expression, may be empty
	Null   string // "notnull", "nullable" or empty
}

// commands are the supported query kinds.
var commands = map[string]bool{
	"one": true, "many": true, "exec": true, "execrows": true, "iter": true,
}

var nameRe = regexp.MustCompile(`^--\s*name:\s*(\S+)\s+:(\S+)\s*$`)

// ParseDir parses every *.sql file in dir, in name order.
func ParseDir(dir string) ([]Query, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("queryfile: %w", err)
	}
	var files []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			files = append(files, filepath.Join(dir, e.Name()))
		}
	}
	sort.Strings(files)
	if len(files) == 0 {
		return nil, fmt.Errorf("queryfile: no .sql files in %s", dir)
	}

	var queries []Query
	for _, path := range files {
		src, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("queryfile: %w", err)
		}
		qs, err := ParseFile(path, src)
		if err != nil {
			return nil, err
		}
		queries = append(queries, qs...)
	}
	return queries, nil
}

// ParseFile splits one annotated .sql source into queries.
func ParseFile(path string, src []byte) ([]Query, error) {
	var queries []Query
	var cur *Query
	inBody := false

	flush := func() error {
		if cur == nil {
			return nil
		}
		cur.SQL = strings.TrimSpace(cur.SQL)
		cur.SQL = strings.TrimSuffix(cur.SQL, ";")
		cur.SQL = strings.TrimRight(cur.SQL, " \t\n")
		if cur.SQL == "" {
			return fmt.Errorf("%s:%d: query %s has no SQL body",
				cur.File, cur.Line, cur.Name)
		}
		queries = append(queries, *cur)
		cur = nil
		return nil
	}

	lines := strings.Split(string(src), "\n")
	for i, line := range lines {
		lineno := i + 1
		trimmed := strings.TrimSpace(line)

		if m := nameRe.FindStringSubmatch(trimmed); m != nil {
			if err := flush(); err != nil {
				return nil, err
			}
			name, cmd := m[1], m[2]
			if !commands[cmd] {
				return nil, fmt.Errorf("%s:%d: unknown command :%s", path, lineno, cmd)
			}
			if !exportedIdent(name) {
				return nil, fmt.Errorf(
					"%s:%d: query name %q must be an exported Go identifier",
					path, lineno, name)
			}
			cur = &Query{
				Name: name, Command: cmd,
				File: path, Line: lineno,
				ParamNames: map[int]string{},
			}
			inBody = false
			continue
		}

		if cur == nil {
			continue // prose or blank lines before the first query
		}

		// Comment lines between the header and the body carry the doc text
		// and annotations. Once the body starts, comments are plain SQL.
		if !inBody && strings.HasPrefix(trimmed, "--") {
			text := strings.TrimSpace(strings.TrimPrefix(trimmed, "--"))
			switch {
			case strings.HasPrefix(text, "override:"):
				o, err := parseOverride(strings.TrimPrefix(text, "override:"))
				if err != nil {
					return nil, fmt.Errorf("%s:%d: %w", path, lineno, err)
				}
				cur.Overrides = append(cur.Overrides, o)
			case strings.HasPrefix(text, "param:"):
				n, name, err := parseParamName(strings.TrimPrefix(text, "param:"))
				if err != nil {
					return nil, fmt.Errorf("%s:%d: %w", path, lineno, err)
				}
				cur.ParamNames[n] = name
			case strings.HasPrefix(text, "embed:"):
				e, err := parseEmbed(strings.TrimPrefix(text, "embed:"))
				if err != nil {
					return nil, fmt.Errorf("%s:%d: %w", path, lineno, err)
				}
				cur.Embeds = append(cur.Embeds, e)
			default:
				if text != "" {
					if cur.Doc != "" {
						cur.Doc += " "
					}
					cur.Doc += text
				}
			}
			continue
		}

		if trimmed == "" && !inBody {
			continue
		}
		inBody = true
		cur.SQL += line + "\n"
	}
	if err := flush(); err != nil {
		return nil, err
	}
	if len(queries) == 0 {
		return nil, fmt.Errorf("%s: no \"-- name:\" queries found", path)
	}
	return queries, nil
}

// parseOverride decodes "<column|$N> [gotype] [notnull|nullable]".
func parseOverride(s string) (Override, error) {
	fields := strings.Fields(s)
	if len(fields) < 2 || len(fields) > 3 {
		return Override{}, fmt.Errorf(
			"override wants \"<column|$N> <go-type> [notnull|nullable]\", got %q", s)
	}

	var o Override
	target := fields[0]
	if strings.HasPrefix(target, "$") {
		n, err := strconv.Atoi(target[1:])
		if err != nil || n < 1 {
			return Override{}, fmt.Errorf("bad parameter %q in override", target)
		}
		o.Param = n
	} else {
		o.Column = target
	}

	rest := fields[1:]
	if last := rest[len(rest)-1]; last == "notnull" || last == "nullable" {
		o.Null = last
		rest = rest[:len(rest)-1]
	}
	if len(rest) == 1 {
		o.GoType = rest[0]
	}
	if o.GoType == "" && o.Null == "" {
		return Override{}, fmt.Errorf("override %q changes nothing", s)
	}
	return o, nil
}

// parseEmbed decodes "<table> [as <Field>]".
func parseEmbed(s string) (Embed, error) {
	fields := strings.Fields(s)
	switch {
	case len(fields) == 1:
		return Embed{Table: fields[0]}, nil
	case len(fields) == 3 && fields[1] == "as" && exportedIdent(fields[2]):
		return Embed{Table: fields[0], As: fields[2]}, nil
	}
	return Embed{}, fmt.Errorf(
		"embed wants \"<table> [as <ExportedField>]\", got %q", s)
}

// parseParamName decodes "$N <name>".
func parseParamName(s string) (int, string, error) {
	fields := strings.Fields(s)
	if len(fields) != 2 || !strings.HasPrefix(fields[0], "$") {
		return 0, "", fmt.Errorf("param wants \"$N <name>\", got %q", s)
	}
	n, err := strconv.Atoi(fields[0][1:])
	if err != nil || n < 1 {
		return 0, "", fmt.Errorf("bad parameter %q", fields[0])
	}
	return n, fields[1], nil
}

// exportedIdent reports whether s is an exported Go identifier.
func exportedIdent(s string) bool {
	if s == "" || s[0] < 'A' || s[0] > 'Z' {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9', r == '_':
		default:
			return false
		}
	}
	return true
}
