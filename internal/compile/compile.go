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

	c := &compiler{
		cfg: cfg, cat: cat,
		models:      map[uint32]gen.Model{},
		enums:       map[uint32]gen.Enum{},
		helpers:     map[string]bool{},
		typeImports: map[string]string{},
		importSel:   map[string]string{},
		selUsed:     map[string]string{},
		importAlias: map[string]string{},
	}
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

	in := gen.Input{
		Package:       cfg.Package,
		TypeImports:   c.typeImports,
		ImportAliases: c.importAlias,
		JSONTags:      cfg.JSONTags,
		EmitInterface: cfg.Interface,
	}
	for _, m := range c.models {
		in.Models = append(in.Models, m)
	}
	sort.Slice(in.Models, func(i, j int) bool {
		return in.Models[i].Name < in.Models[j].Name
	})
	for _, e := range c.enums {
		in.Enums = append(in.Enums, e)
	}
	sort.Slice(in.Enums, func(i, j int) bool {
		return in.Enums[i].Name < in.Enums[j].Name
	})
	for h := range c.helpers {
		in.ArrayHelpers = append(in.ArrayHelpers, h)
	}
	sort.Strings(in.ArrayHelpers)
	for _, key := range fileOrder {
		in.Files = append(in.Files, *files[key])
	}

	if err := checkNameCollisions(in); err != nil {
		return nil, err
	}

	rendered, err := gen.Render(in)
	if err != nil {
		return nil, err
	}
	return &Result{Files: rendered, Warnings: c.warnings}, nil
}

// checkNameCollisions rejects generated packages where two type names
// collide - two same-named tables from different schemas, a model named
// like a row struct, and so on. Colliding silently would produce invalid Go.
func checkNameCollisions(in gen.Input) error {
	owners := map[string]string{
		"DBTX": "the generated plumbing", "Queries": "the generated plumbing",
		"Querier": "the generated plumbing",
	}
	claim := func(name, owner string) error {
		if prev, ok := owners[name]; ok {
			return fmt.Errorf(
				"generated name %s collides: %s and %s; rename one, e.g. "+
					`"rename": {"schema.table": "Other"}`, name, prev, owner)
		}
		owners[name] = owner
		return nil
	}

	for _, m := range in.Models {
		if err := claim(m.Name, "the model of table "+m.Table); err != nil {
			return err
		}
	}
	for _, e := range in.Enums {
		if err := claim(e.Name, "the enum "+e.DBName); err != nil {
			return err
		}
		// The enum's companions are package-level names too.
		if err := claim(e.Name+"Values", "the values list of enum "+e.DBName); err != nil {
			return err
		}
		if err := claim("Parse"+e.Name, "the parser of enum "+e.DBName); err != nil {
			return err
		}
	}
	for _, f := range in.Files {
		for _, q := range f.Queries {
			if q.Ret.Kind == gen.RetRow {
				if err := claim(q.Ret.Type, "the row struct of "+q.Name); err != nil {
					return err
				}
			}
			if len(q.Params) >= 4 {
				if err := claim(q.Name+"Params", "the params struct of "+q.Name); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

type compiler struct {
	cfg         config.Config
	cat         *catalog.Catalog
	models      map[uint32]gen.Model // tables emitted as model structs
	enums       map[uint32]gen.Enum  // enum types emitted alongside models
	helpers     map[string]bool      // array adapters in use
	typeImports map[string]string    // custom type expr to import path
	importSel   map[string]string    // import path to chosen package selector
	selUsed     map[string]string    // selector to the import path that owns it
	importAlias map[string]string    // import path to alias (only when needed)
	warnings    []string
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
	case "one", "many", "iter":
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
		expr, helper, err := c.goType(oid, true, override)
		if err != nil {
			return nil, fmt.Errorf("parameter $%d: %w", n, err)
		}
		params = append(params, gen.Param{Name: names[n], Type: expr, Helper: helper})
	}
	return params, nil
}

// compileRet decides between no result, a scalar, a full-table model and a
// per-query row struct, honoring "-- embed:" annotations.
func (c *compiler) compileRet(q qfQuery, st *pgwire.Statement) (gen.Ret, error) {
	cols := st.Columns
	if len(cols) == 0 {
		return gen.Ret{Kind: gen.RetNone}, nil
	}

	if len(cols) == 1 && len(q.Embeds) == 0 {
		expr, helper, err := c.columnType(q, cols[0])
		if err != nil {
			return gen.Ret{}, err
		}
		return gen.Ret{Kind: gen.RetScalar, Type: expr, Helper: helper}, nil
	}

	if len(q.Embeds) == 0 {
		if table, ok := c.sameTable(cols); ok && isPermutation(cols, table) {
			model, fields, err := c.selectOrderedFields(cols, table)
			if err != nil {
				return gen.Ret{}, err
			}
			return gen.Ret{Kind: gen.RetModel, Type: model.Name, Fields: fields}, nil
		}
	}

	// A projection: build the per-query row struct, folding embedded runs.
	embeds := q.Embeds
	seen := map[string]bool{}
	var fields []gen.Field
	addName := func(goName string) error {
		if seen[goName] {
			return fmt.Errorf("duplicate result field %s; disambiguate "+
				"with AS or \"embed: ... as <Name>\"", goName)
		}
		seen[goName] = true
		return nil
	}

	for i := 0; i < len(cols); {
		if len(embeds) > 0 {
			table, err := c.tableByName(embeds[0].Table)
			if err != nil {
				return gen.Ret{}, err
			}
			if n := len(table.Columns); i+n <= len(cols) && isPermutation(cols[i:i+n], table) {
				model, children, err := c.selectOrderedFields(cols[i:i+n], table)
				if err != nil {
					return gen.Ret{}, err
				}
				goName := embeds[0].As
				if goName == "" {
					goName = model.Name
				}
				if err := addName(goName); err != nil {
					return gen.Ret{}, err
				}
				fields = append(fields, gen.Field{
					Name:   table.Name,
					GoName: goName,
					Type:   model.Name,
					Embed:  children,
				})
				i += n
				embeds = embeds[1:]
				continue
			}
		}

		col := cols[i]
		if col.Name == "" || col.Name == "?column?" {
			return gen.Ret{}, fmt.Errorf(
				"a result column has no name; give it one with AS")
		}
		if err := addName(gen.CamelCase(col.Name)); err != nil {
			return gen.Ret{}, err
		}
		expr, helper, err := c.columnType(q, col)
		if err != nil {
			return gen.Ret{}, err
		}
		fields = append(fields, gen.Field{Name: col.Name, Type: expr, Helper: helper})
		i++
	}
	if len(embeds) > 0 {
		return gen.Ret{}, fmt.Errorf(
			"embed %s: the result has no contiguous run of that table's "+
				"full column set (all columns, side by side, any order); "+
				"selecting them with .* always works", embeds[0].Table)
	}
	return gen.Ret{Kind: gen.RetRow, Type: q.Name + "Row", Fields: fields}, nil
}

// tableByName finds a loaded table by name, optionally schema-qualified.
func (c *compiler) tableByName(name string) (*catalog.Table, error) {
	var found *catalog.Table
	for _, t := range c.cat.Tables() {
		if t.Name == name || t.Schema+"."+t.Name == name {
			if found != nil {
				return nil, fmt.Errorf(
					"embed %s is ambiguous; qualify it as schema.table", name)
			}
			found = t
		}
	}
	if found == nil {
		return nil, fmt.Errorf(
			"embed %s: the query returns no columns of that table", name)
	}
	return found, nil
}

// sameTable returns the loaded table when every column originates from the
// same one.
func (c *compiler) sameTable(cols []pgwire.Column) (*catalog.Table, bool) {
	first := cols[0].TableOID
	if first == 0 {
		return nil, false
	}
	for _, col := range cols {
		if col.TableOID != first {
			return nil, false
		}
	}
	return c.cat.Table(first)
}

// isPermutation reports whether cols is exactly table's full column set -
// every column present once, in any order. Column order in a SELECT is
// irrelevant on purpose: after ALTER TABLE ADD COLUMN the physical attnum
// order no longer matches what a human writes.
func isPermutation(cols []pgwire.Column, table *catalog.Table) bool {
	if len(cols) != len(table.Columns) {
		return false
	}
	byAttnum := make(map[int16]string, len(table.Columns))
	for _, tc := range table.Columns {
		byAttnum[tc.Attnum] = tc.Name
	}
	for _, col := range cols {
		name, ok := byAttnum[col.Attnum]
		if col.TableOID != table.OID || !ok || name != col.Name {
			return false
		}
		delete(byAttnum, col.Attnum)
	}
	return true
}

// selectOrderedFields builds the scan fields for a full-table match in the
// SELECT's own order, each mapped onto the model's field of the same column.
func (c *compiler) selectOrderedFields(
	cols []pgwire.Column,
	table *catalog.Table,
) (gen.Model, []gen.Field, error) {
	model, err := c.modelFor(table)
	if err != nil {
		return gen.Model{}, nil, err
	}
	byName := make(map[string]gen.Field, len(model.Fields))
	for _, f := range model.Fields {
		byName[f.Name] = f
	}
	fields := make([]gen.Field, 0, len(cols))
	for _, col := range cols {
		fields = append(fields, byName[col.Name])
	}
	return model, fields, nil
}

// modelFor returns (building on first use) the model struct of a table.
// The rename map is consulted with the schema-qualified name first, then
// the bare table name; without an entry the CamelCase of the table name is
// used as-is.
func (c *compiler) modelFor(table *catalog.Table) (gen.Model, error) {
	if m, ok := c.models[table.OID]; ok {
		return m, nil
	}

	name := c.cfg.Rename[table.Schema+"."+table.Name]
	if name == "" {
		name = c.cfg.Rename[table.Name]
	}
	if name == "" {
		name = gen.CamelCase(table.Name)
	}
	display := table.Name
	if table.Schema != "public" {
		display = table.Schema + "." + table.Name
	}
	m := gen.Model{Name: name, Table: display}
	for _, col := range table.Columns {
		expr, helper, err := c.goType(col.TypeOID, col.NotNull, nil)
		if err != nil {
			return gen.Model{}, fmt.Errorf("table %s, column %s: %w",
				display, col.Name, err)
		}
		m.Fields = append(m.Fields, gen.Field{Name: col.Name, Type: expr, Helper: helper})
	}
	c.models[table.OID] = m
	return m, nil
}

// columnType resolves one result column, honoring its override and the
// catalog's attnotnull when the column has a table origin.
func (c *compiler) columnType(q qfQuery, col pgwire.Column) (string, string, error) {
	notNull := false
	if col.TableOID != 0 {
		notNull = c.cat.NotNull(col.TableOID, col.Attnum)
	}
	expr, helper, err := c.goType(col.TypeOID, notNull, overrideForColumn(q, col.Name))
	if err != nil {
		return "", "", fmt.Errorf("column %q: %w", col.Name, err)
	}
	return expr, helper, nil
}
