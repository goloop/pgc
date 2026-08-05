package gen

import (
	"fmt"
	"sort"
	"strings"
)

// emitQuerier renders querier.go: an interface with one method per query,
// satisfied by *Queries, so callers can substitute a test double.
func emitQuerier(in Input) string {
	var queries []Query
	base := []string{"context"}
	var types []string
	for _, f := range in.Files {
		for _, q := range f.Queries {
			queries = append(queries, q)
			if q.Command == "iter" {
				base = append(base, "iter")
			}
			// Only what the signatures spell out matters here: parameter
			// types and scalar results. Row and model structs appear by
			// name alone - their field types must not drag imports in.
			//
			// Past four parameters the signature collapses to an
			// XxxParams struct, so the individual types stop appearing
			// and must not be counted either - otherwise a query with,
			// say, a json.RawMessage parameter imports encoding/json
			// into a file that never spells it out, and the generated
			// package does not compile.
			if !q.UsesParamStruct() {
				for _, p := range q.Params {
					types = append(types, p.Type)
				}
			}
			if q.Ret.Kind == RetScalar {
				types = append(types, q.Ret.Type)
			}
		}
	}
	sort.Slice(queries, func(i, j int) bool {
		return queries[i].Name < queries[j].Name
	})

	var b strings.Builder
	b.WriteString(header)
	fmt.Fprintf(&b, "\npackage %s\n", in.Package)
	b.WriteString(emitImports(in, base, types))

	b.WriteString(`
// Querier is the interface version of Queries: one method per SQL query.
// *Queries satisfies it; substitute your own implementation in tests.
type Querier interface {
`)
	for i, q := range queries {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString(indentComment(wrapComment(q.Doc)))
		fmt.Fprintf(&b, "\t%s(ctx context.Context%s) %s\n",
			q.Name, querierParams(in.Namer, q), resultOf(q))
	}
	b.WriteString("}\n\nvar _ Querier = (*Queries)(nil)\n")
	return b.String()
}

// querierParams renders a query's parameter list after ctx, matching the
// generated method exactly.
func querierParams(n *Namer, q Query) string {
	var names []string
	for _, p := range q.Params {
		names = append(names, n.paramName(p.Name))
	}
	return signatureParams(q.Params, names, q.UsesParamStruct(), q.Name)
}

// resultOf renders a query's result list.
func resultOf(q Query) string {
	switch q.Command {
	case "execrows":
		return "(int64, error)"
	case "one":
		return "(" + q.Ret.Type + ", error)"
	case "many":
		return "([]" + q.Ret.Type + ", error)"
	case "iter":
		return "iter.Seq2[" + q.Ret.Type + ", error]"
	default: // exec
		return "error"
	}
}

// indentComment shifts // comment lines one tab right, for interface bodies.
func indentComment(c string) string {
	lines := strings.Split(strings.TrimRight(c, "\n"), "\n")
	for i, l := range lines {
		lines[i] = "\t" + l
	}
	return strings.Join(lines, "\n") + "\n"
}
