package gen

import (
	"fmt"
	"strings"
)

// arrayKind describes one emitted array adapter.
type arrayKind struct {
	elem    string // Go element type
	pg      string // PostgreSQL column type, for the doc comment
	parse   string // loop body converting element e (string) into out[i]
	format  string // loop body converting element v into parts[i]
	strconv bool   // whether the blocks use strconv
}

// arrayKinds is every adapter pgc can emit, keyed by the helper type name.
var arrayKinds = map[string]arrayKind{
	"boolArray": {
		elem: "bool", pg: "boolean[]",
		parse: `		switch e {
		case "t", "true":
			out[i] = true
		case "f", "false":
			out[i] = false
		default:
			return fmt.Errorf("%s: element %d: %q", "%N", i, e)
		}`,
		format: `		if v {
			parts[i] = "t"
		} else {
			parts[i] = "f"
		}`,
	},
	"int16Array": {
		elem: "int16", pg: "smallint[]",
		parse:   intParse("int16", 16),
		format:  `		parts[i] = strconv.FormatInt(int64(v), 10)`,
		strconv: true,
	},
	"int32Array": {
		elem: "int32", pg: "integer[]",
		parse:   intParse("int32", 32),
		format:  `		parts[i] = strconv.FormatInt(int64(v), 10)`,
		strconv: true,
	},
	"int64Array": {
		elem: "int64", pg: "bigint[]",
		parse:   intParse("int64", 64),
		format:  `		parts[i] = strconv.FormatInt(int64(v), 10)`,
		strconv: true,
	},
	"float32Array": {
		elem: "float32", pg: "real[]",
		parse:   floatParse("float32", 32),
		format:  `		parts[i] = strconv.FormatFloat(float64(v), 'g', -1, 32)`,
		strconv: true,
	},
	"float64Array": {
		elem: "float64", pg: "double precision[]",
		parse:   floatParse("float64", 64),
		format:  `		parts[i] = strconv.FormatFloat(float64(v), 'g', -1, 64)`,
		strconv: true,
	},
	"stringArray": {
		elem: "string", pg: "text-like[]",
		parse: `		out[i] = e`,
		format: `		if strings.IndexByte(v, 0) >= 0 {
			return nil, fmt.Errorf(
				"stringArray: element %d holds a NUL byte, which PostgreSQL text cannot store", i)
		}
		parts[i] = pgQuoteElem(v)`,
	},
}

func intParse(typ string, bits int) string {
	return fmt.Sprintf(`		v, err := strconv.ParseInt(e, 10, %d)
		if err != nil {
			return fmt.Errorf("%%s: element %%d: %%w", "%%N", i, err)
		}
		out[i] = %s(v)`, bits, typ)
}

func floatParse(typ string, bits int) string {
	return fmt.Sprintf(`		v, err := strconv.ParseFloat(e, %d)
		if err != nil {
			return fmt.Errorf("%%s: element %%d: %%w", "%%N", i, err)
		}
		out[i] = %s(v)`, bits, typ)
}

// emitArrays renders pgarray.go: the shared text-format codec plus one
// adapter per used helper.
func emitArrays(in Input) string {
	needStrconv := false
	for _, name := range in.ArrayHelpers {
		if arrayKinds[name].strconv {
			needStrconv = true
		}
	}

	var b strings.Builder
	b.WriteString(header)
	fmt.Fprintf(&b, "\npackage %s\n", in.Package)
	b.WriteString("\nimport (\n\t\"database/sql/driver\"\n\t\"fmt\"\n")
	if needStrconv {
		b.WriteString("\t\"strconv\"\n")
	}
	b.WriteString("\t\"strings\"\n)\n")

	b.WriteString(arrayCore)

	for _, name := range in.ArrayHelpers {
		k, ok := arrayKinds[name]
		if !ok {
			continue
		}
		parse := strings.ReplaceAll(k.parse, "%N", name)
		fmt.Fprintf(&b, `
// %s adapts []%s to a PostgreSQL %s column or parameter.
type %s []%s

// Scan implements sql.Scanner for the one-dimensional text format.
func (a *%s) Scan(src any) error {
	elems, err := pgArrayElems(src, %q)
	if err != nil || elems == nil {
		*a = nil
		return err
	}
	out := make([]%s, len(elems))
	for i, e := range elems {
%s
	}
	*a = out
	return nil
}

// Value implements driver.Valuer, encoding the text form; a nil slice is
// SQL NULL.
func (a %s) Value() (driver.Value, error) {
	if a == nil {
		return nil, nil
	}
	parts := make([]string, len(a))
	for i, v := range a {
%s
	}
	return "{" + strings.Join(parts, ",") + "}", nil
}
`,
			name, k.elem, k.pg, name, k.elem,
			name, name, k.elem, parse,
			name, k.format)
	}
	return b.String()
}

// arrayCore is the shared decoder and encoder the adapters build on.
const arrayCore = `
// pgArrayElems decodes the text form of a one-dimensional PostgreSQL array
// into raw element strings. It returns (nil, nil) for SQL NULL. NULL
// elements and multidimensional arrays are rejected: map the column to a
// plain string with an override when you need to carry them.
func pgArrayElems(src any, who string) ([]string, error) {
	var s string
	switch v := src.(type) {
	case nil:
		return nil, nil
	case []byte:
		s = string(v)
	case string:
		s = v
	default:
		return nil, fmt.Errorf("%s: cannot scan %T", who, src)
	}
	if strings.HasPrefix(s, "[") {
		return nil, fmt.Errorf(
			"%s: arrays with explicit bounds are not supported", who)
	}
	if len(s) < 2 || s[0] != '{' || s[len(s)-1] != '}' {
		return nil, fmt.Errorf("%s: malformed array %q", who, s)
	}
	body := s[1 : len(s)-1]
	if body == "" {
		return []string{}, nil
	}

	var elems []string
	var cur strings.Builder
	quoted, wasQuoted := false, false
	flush := func() error {
		e := cur.String()
		if !wasQuoted {
			e = strings.TrimSpace(e)
			if strings.EqualFold(e, "null") {
				return fmt.Errorf(
					"%s: the array has a NULL element; map the column to "+
						"string with an override to handle it", who)
			}
		}
		elems = append(elems, e)
		cur.Reset()
		wasQuoted = false
		return nil
	}
	for i := 0; i < len(body); i++ {
		c := body[i]
		switch {
		case quoted:
			if c == '\\' && i+1 < len(body) {
				i++
				cur.WriteByte(body[i])
			} else if c == '"' {
				quoted = false
			} else {
				cur.WriteByte(c)
			}
		case c == '"':
			quoted = true
			wasQuoted = true
		case c == '{':
			return nil, fmt.Errorf(
				"%s: multidimensional arrays are not supported", who)
		case c == ',':
			if err := flush(); err != nil {
				return nil, err
			}
		default:
			cur.WriteByte(c)
		}
	}
	if err := flush(); err != nil {
		return nil, err
	}
	return elems, nil
}

// pgQuoteElem renders one string element of an array literal. Every element
// is quoted: unquoted, the server trims surrounding whitespace - newlines and
// form feeds included - and reads NULL as a null, so only the quoted form
// carries the bytes through unchanged.
func pgQuoteElem(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 2)
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		if s[i] == '"' || s[i] == '\\' {
			b.WriteByte('\\')
		}
		b.WriteByte(s[i])
	}
	b.WriteByte('"')
	return b.String()
}
`
