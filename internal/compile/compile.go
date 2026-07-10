// Package compile orchestrates one pgc run: parse the query files, ask the
// live database to describe every statement, resolve OIDs through the
// catalog, decide names and Go types, and hand a fully resolved model to
// gen. All policy lives here; gen only renders.
package compile

import (
	"fmt"
	"path/filepath"
	"regexp"
	"sort"

	"github.com/goloop/pgc/internal/catalog"
	"github.com/goloop/pgc/internal/config"
	"github.com/goloop/pgc/internal/gen"
	"github.com/goloop/pgc/internal/pgwire"
)

// DB is the slice of pgwire.Conn that compilation needs; tests substitute a
// fake.
type DB interface {
	Describe(query string) (*pgwire.Statement, error)
	Query(sql string) ([][]pgwire.Value, error)
}

// Result carries the rendered files plus any warnings worth showing.
type Result struct {
	Files    []gen.OutFile
	Warnings []string
}

// Run compiles every query in cfg.Queries against the database behind db.
func Run(db DB, cfg config.Config) (*Result, error) {
	queries, err := queryfileParse(cfg.Queries)
	if err != nil {
		return nil, err
	}

	// Describe everything first, collecting the OIDs one catalog pass
	// resolves.
	type described struct {
		q  qfQuery
		st *pgwire.Statement
	}
	var ds []described
	var typeOIDs, tableOIDs []uint32
	for _, q := range queries {
		st, err := db.Describe(q.SQL)
		if err != nil {
			return nil, fmt.Errorf("%s:%d: %s: %w", q.File, q.Line, q.Name, err)
		}
		typeOIDs = append(typeOIDs, st.ParamOIDs...)
		for _, col := range st.Columns {
			typeOIDs = append(typeOIDs, col.TypeOID)
			if col.TableOID != 0 {
				tableOIDs = append(tableOIDs, col.TableOID)
			}
		}
		ds = append(ds, described{q, st})
	}

	cat, err := catalog.Load(db, typeOIDs, tableOIDs)
	if err != nil {
		return nil, err
	}

	c := &compiler{cfg: cfg, cat: cat, models: map[uint32]gen.Model{}}
	files := map[string]*gen.SrcFile{}
	var fileOrder []string
	for _, d := range ds {
		gq, err := c.compileQuery(d.q, d.st)
		if err != nil {
			return nil, err
		}
		key := d.q.File
		if files[key] == nil {
			base := filepath.Base(key)
			files[key] = &gen.SrcFile{
				Source: filepath.ToSlash(key),
				Out:    base + ".go",
			}
			fileOrder = append(fileOrder, key)
		}
		files[key].Queries = append(files[key].Queries, gq)
	}

	in := gen.Input{Package: cfg.Package}
	for _, m := range c.models {
		in.Models = append(in.Models, m)
	}
	sort.Slice(in.Models, func(i, j int) bool {
		return in.Models[i].Name < in.Models[j].Name
	})
	for _, key := range fileOrder {
		in.Files = append(in.Files, *files[key])
	}

	rendered, err := gen.Render(in)
	if err != nil {
		return nil, err
	}
	return &Result{Files: rendered, Warnings: c.warnings}, nil
}

type compiler struct {
	cfg      config.Config
	cat      *catalog.Catalog
	models   map[uint32]gen.Model // tables emitted as model structs
	warnings []string
}

var outerJoinRe = regexp.MustCompile(`(?i)\b(left|right|full)\s+(outer\s+)?join\b`)

// compileQuery resolves one query's parameters, result shape and doc.
func (c *compiler) compileQuery(q qfQuery, st *pgwire.Statement) (gen.Query, error) {
	fail := func(err error) (gen.Query, error) {
		return gen.Query{}, fmt.Errorf("%s:%d: %s: %w", q.File, q.Line, q.Name, err)
	}

	var colNames []string
	for _, col := range st.Columns {
		colNames = append(colNames, col.Name)
	}
	if err := checkOverrides(q, len(st.ParamOIDs), colNames); err != nil {
		return fail(err)
	}

	params, err := c.compileParams(q, st)
	if err != nil {
		return fail(err)
	}
	ret, err := c.compileRet(q, st)
	if err != nil {
		return fail(err)
	}

	switch q.Command {
	case "one", "many":
		if ret.Kind == gen.RetNone {
			return fail(fmt.Errorf("statement returns no rows; use :exec"))
		}
	}

	if outerJoinRe.MatchString(q.SQL) {
		c.warnings = append(c.warnings, fmt.Sprintf(
			"%s:%d: %s uses an outer join; the catalog cannot see which side "+
				"is nullable - add \"-- override: <column> nullable\" for columns "+
				"from the outer side", q.File, q.Line, q.Name))
	}

	return gen.Query{
		Name:    q.Name,
		Doc:     docFor(q),
		Command: q.Command,
		SQL:     q.SQL,
		Params:  params,
		Ret:     ret,
	}, nil
}

// compileParams names and types the $N parameters. Parameters default to
// NOT NULL; an override can hand in a pointer type.
func (c *compiler) compileParams(q qfQuery, st *pgwire.Statement) ([]gen.Param, error) {
	names := inferParamNames(q, len(st.ParamOIDs))
	params := make([]gen.Param, 0, len(st.ParamOIDs))
	for i, oid := range st.ParamOIDs {
		n := i + 1
		override := overrideForParam(q, n)
		expr, err := c.goType(oid, true, override)
		if err != nil {
			return nil, fmt.Errorf("parameter $%d: %w", n, err)
		}
		params = append(params, gen.Param{Name: names[n], Type: expr})
	}
	return params, nil
}

// compileRet decides between no result, a scalar, a full-table model and a
// per-query row struct.
func (c *compiler) compileRet(q qfQuery, st *pgwire.Statement) (gen.Ret, error) {
	cols := st.Columns
	if len(cols) == 0 {
		return gen.Ret{Kind: gen.RetNone}, nil
	}

	if len(cols) == 1 {
		expr, err := c.columnType(q, cols[0])
		if err != nil {
			return gen.Ret{}, err
		}
		return gen.Ret{Kind: gen.RetScalar, Type: expr}, nil
	}

	if table, ok := c.fullTableMatch(cols); ok {
		model, err := c.modelFor(table)
		if err != nil {
			return gen.Ret{}, err
		}
		return gen.Ret{Kind: gen.RetModel, Type: model.Name, Fields: model.Fields}, nil
	}

	// A projection: build the per-query row struct.
	seen := map[string]bool{}
	fields := make([]gen.Field, 0, len(cols))
	for _, col := range cols {
		if col.Name == "" || col.Name == "?column?" {
			return gen.Ret{}, fmt.Errorf(
				"a result column has no name; give it one with AS")
		}
		if seen[col.Name] {
			return gen.Ret{}, fmt.Errorf(
				"duplicate result column %q; disambiguate with AS", col.Name)
		}
		seen[col.Name] = true
		expr, err := c.columnType(q, col)
		if err != nil {
			return gen.Ret{}, err
		}
		fields = append(fields, gen.Field{Name: col.Name, Type: expr})
	}
	return gen.Ret{Kind: gen.RetRow, Type: q.Name + "Row", Fields: fields}, nil
}

// fullTableMatch reports whether the columns are exactly one table's columns
// in attnum order - the case where the model struct is reused instead of a
// row struct.
func (c *compiler) fullTableMatch(cols []pgwire.Column) (*catalog.Table, bool) {
	first := cols[0].TableOID
	if first == 0 {
		return nil, false
	}
	table, ok := c.cat.Table(first)
	if !ok || len(cols) != len(table.Columns) {
		return nil, false
	}
	for i, col := range cols {
		tc := table.Columns[i]
		if col.TableOID != first || col.Attnum != tc.Attnum || col.Name != tc.Name {
			return nil, false
		}
	}
	return table, true
}

// modelFor returns (building on first use) the model struct of a table.
func (c *compiler) modelFor(table *catalog.Table) (gen.Model, error) {
	if m, ok := c.models[table.OID]; ok {
		return m, nil
	}

	name := c.cfg.Rename[table.Name]
	if name == "" {
		name = gen.CamelCase(table.Name)
	}
	m := gen.Model{Name: name, Table: table.Name}
	for _, col := range table.Columns {
		expr, err := c.goType(col.TypeOID, col.NotNull, nil)
		if err != nil {
			return gen.Model{}, fmt.Errorf("table %s, column %s: %w",
				table.Name, col.Name, err)
		}
		m.Fields = append(m.Fields, gen.Field{Name: col.Name, Type: expr})
	}
	c.models[table.OID] = m
	return m, nil
}

// columnType resolves one result column, honoring its override and the
// catalog's attnotnull when the column has a table origin.
func (c *compiler) columnType(q qfQuery, col pgwire.Column) (string, error) {
	notNull := false
	if col.TableOID != 0 {
		notNull = c.cat.NotNull(col.TableOID, col.Attnum)
	}
	expr, err := c.goType(col.TypeOID, notNull, overrideForColumn(q, col.Name))
	if err != nil {
		return "", fmt.Errorf("column %q: %w", col.Name, err)
	}
	return expr, nil
}
