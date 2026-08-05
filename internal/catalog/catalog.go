// Package catalog resolves what Describe reports - bare OIDs - into names
// and nullability, by reading pg_type, pg_class and pg_attribute over the
// same connection. The live database is the single source of truth; nothing
// here guesses.
package catalog

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/goloop/pgc/internal/pgwire"
)

// Querier is the one pgwire method the catalog needs, split out so tests
// can substitute a fake.
type Querier interface {
	Query(sql string) ([][]pgwire.Value, error)
}

// Type is one pg_type row.
type Type struct {
	OID      uint32 `json:"oid"`
	Name     string `json:"name"`           // typname, e.g. int8
	Kind     byte   `json:"kind"`           // typtype: b base, e enum, d domain
	Category byte   `json:"category"`       // typcategory: A marks arrays
	Elem     uint32 `json:"elem,omitempty"` // element type OID for arrays
	Base     uint32 `json:"base,omitempty"` // base type OID for domains
}

// Table is one pg_class relation with its live columns.
type Table struct {
	OID     uint32   `json:"oid"`
	Schema  string   `json:"schema"` // namespace name, e.g. public
	Name    string   `json:"name"`   // relname
	Columns []Column `json:"columns"`
}

// Column is one pg_attribute row of a table, in attnum order.
type Column struct {
	Name    string `json:"name"`
	Attnum  int16  `json:"attnum"`
	TypeOID uint32 `json:"type"`
	NotNull bool   `json:"notnull,omitempty"`
}

// Catalog is the loaded slice of the system catalogs that the given
// statements touch.
type Catalog struct {
	types  map[uint32]Type
	tables map[uint32]*Table
	enums  map[uint32][]string // enum type OID to its labels, in sort order
}

// Load fetches the named type and table OIDs in three batched queries.
// Tables are loaded first so their column types join the type lookup - a
// model may use types no query result mentions directly.
func Load(q Querier, typeOIDs, tableOIDs []uint32) (*Catalog, error) {
	c := &Catalog{
		types:  map[uint32]Type{},
		tables: map[uint32]*Table{},
		enums:  map[uint32][]string{},
	}
	if err := c.loadTables(q, tableOIDs); err != nil {
		return nil, err
	}
	if err := c.loadTypes(q, append(typeOIDs, c.ColumnTypeOIDs()...)); err != nil {
		return nil, err
	}
	if err := c.loadEnums(q); err != nil {
		return nil, err
	}
	return c, nil
}

// loadEnums fetches the labels of every loaded enum type, in their declared
// order.
func (c *Catalog) loadEnums(q Querier) error {
	var oids []uint32
	for oid, t := range c.types {
		if t.Kind == 'e' {
			oids = append(oids, oid)
		}
	}
	list := oidList(oids)
	if list == "" {
		return nil
	}
	rows, err := q.Query(
		"SELECT enumtypid, enumlabel FROM pg_catalog.pg_enum " +
			"WHERE enumtypid IN (" + list + ") ORDER BY enumtypid, enumsortorder")
	if err != nil {
		return fmt.Errorf("catalog: enums: %w", err)
	}
	for _, row := range rows {
		oid, err := parseOID(row[0])
		if err != nil {
			return err
		}
		c.enums[oid] = append(c.enums[oid], row[1].S)
	}
	return nil
}

// EnumLabels returns the labels of an enum type OID, or nil for other types.
func (c *Catalog) EnumLabels(oid uint32) []string {
	return c.enums[oid]
}

// IsEnum reports whether an OID is an enum type.
func (c *Catalog) IsEnum(oid uint32) bool {
	return c.types[oid].Kind == 'e'
}

// TypeName returns the pg_type name of an OID.
func (c *Catalog) TypeName(oid uint32) (string, bool) {
	t, ok := c.types[oid]
	return t.Name, ok
}

// Table returns a loaded table by its pg_class OID.
func (c *Catalog) Table(oid uint32) (*Table, bool) {
	t, ok := c.tables[oid]
	return t, ok
}

// Tables returns every loaded table, in OID order.
func (c *Catalog) Tables() []*Table {
	tables := make([]*Table, 0, len(c.tables))
	for _, t := range c.tables {
		tables = append(tables, t)
	}
	sort.Slice(tables, func(i, j int) bool { return tables[i].OID < tables[j].OID })
	return tables
}

// NotNull reports pg_attribute.attnotnull for one table column.
func (c *Catalog) NotNull(table uint32, attnum int16) bool {
	t, ok := c.tables[table]
	if !ok {
		return false
	}
	for _, col := range t.Columns {
		if col.Attnum == attnum {
			return col.NotNull
		}
	}
	return false
}

// loadTypes fetches type rows, then follows array element and domain base
// references until the set is closed, so an "_int4" always brings its
// "int4" along.
func (c *Catalog) loadTypes(q Querier, oids []uint32) error {
	pending := oids
	for round := 0; len(pending) > 0; round++ {
		if round > 8 {
			return fmt.Errorf("catalog: type reference chain too deep")
		}
		list := oidList(pending)
		if list == "" {
			return nil
		}
		rows, err := q.Query(
			"SELECT oid, typname, typtype, typcategory, typelem, typbasetype " +
				"FROM pg_catalog.pg_type WHERE oid IN (" + list + ")")
		if err != nil {
			return fmt.Errorf("catalog: types: %w", err)
		}
		for _, row := range rows {
			oid, err := parseOID(row[0])
			if err != nil {
				return err
			}
			t := Type{OID: oid, Name: row[1].S}
			if row[2].S != "" {
				t.Kind = row[2].S[0]
			}
			if row[3].S != "" {
				t.Category = row[3].S[0]
			}
			if t.Elem, err = parseOID(row[4]); err != nil {
				return err
			}
			if t.Base, err = parseOID(row[5]); err != nil {
				return err
			}
			c.types[oid] = t
		}
		for _, oid := range pending {
			if _, ok := c.types[oid]; !ok {
				return fmt.Errorf("catalog: unknown type oid %d", oid)
			}
		}

		// Follow references that are not loaded yet.
		var next []uint32
		for _, t := range c.types {
			for _, ref := range []uint32{t.Elem, t.Base} {
				if ref != 0 {
					if _, ok := c.types[ref]; !ok {
						next = append(next, ref)
					}
				}
			}
		}
		pending = next
	}
	return nil
}

// Type returns the full pg_type row of an OID.
func (c *Catalog) Type(oid uint32) (Type, bool) {
	t, ok := c.types[oid]
	return t, ok
}

func (c *Catalog) loadTables(q Querier, oids []uint32) error {
	list := oidList(oids)
	if list == "" {
		return nil
	}
	rows, err := q.Query(
		"SELECT c.oid, c.relname, n.nspname FROM pg_catalog.pg_class c " +
			"JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace " +
			"WHERE c.oid IN (" + list + ")")
	if err != nil {
		return fmt.Errorf("catalog: tables: %w", err)
	}
	for _, row := range rows {
		oid, err := parseOID(row[0])
		if err != nil {
			return err
		}
		c.tables[oid] = &Table{OID: oid, Name: row[1].S, Schema: row[2].S}
	}
	for _, oid := range oids {
		if _, ok := c.tables[oid]; !ok {
			return fmt.Errorf("catalog: unknown table oid %d", oid)
		}
	}

	rows, err = q.Query(
		"SELECT attrelid, attnum, attname, atttypid, attnotnull " +
			"FROM pg_catalog.pg_attribute " +
			"WHERE attrelid IN (" + list + ") AND attnum > 0 AND NOT attisdropped " +
			"ORDER BY attrelid, attnum")
	if err != nil {
		return fmt.Errorf("catalog: columns: %w", err)
	}
	for _, row := range rows {
		rel, err := parseOID(row[0])
		if err != nil {
			return err
		}
		attnum, err := strconv.Atoi(row[1].S)
		if err != nil {
			return fmt.Errorf("catalog: attnum %q", row[1].S)
		}
		typ, err := parseOID(row[3])
		if err != nil {
			return err
		}
		c.tables[rel].Columns = append(c.tables[rel].Columns, Column{
			Name:    row[2].S,
			Attnum:  int16(attnum),
			TypeOID: typ,
			NotNull: row[4].S == "t",
		})
	}
	return nil
}

// ColumnTypeOIDs returns every distinct column type of the loaded tables,
// so model fields can be resolved through the same type map.
func (c *Catalog) ColumnTypeOIDs() []uint32 {
	seen := map[uint32]bool{}
	for _, t := range c.tables {
		for _, col := range t.Columns {
			seen[col.TypeOID] = true
		}
	}
	oids := make([]uint32, 0, len(seen))
	for oid := range seen {
		oids = append(oids, oid)
	}
	sort.Slice(oids, func(i, j int) bool { return oids[i] < oids[j] })
	return oids
}

// oidList renders distinct OIDs as "1,2,3". OIDs come from the server, never
// from user input.
func oidList(oids []uint32) string {
	seen := map[uint32]bool{}
	var list []string
	for _, oid := range oids {
		if oid != 0 && !seen[oid] {
			seen[oid] = true
			list = append(list, strconv.FormatUint(uint64(oid), 10))
		}
	}
	sort.Strings(list)
	return strings.Join(list, ",")
}

func parseOID(v pgwire.Value) (uint32, error) {
	n, err := strconv.ParseUint(v.S, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("catalog: bad oid %q", v.S)
	}
	return uint32(n), nil
}

// Snapshot is everything a Catalog holds, in a form that can be written down
// and read back. It exists so a generation run can work from a recorded
// schema instead of a live server: the same values, no connection.
type Snapshot struct {
	Types  []Type              `json:"types"`
	Tables []*Table            `json:"tables"`
	Enums  map[string][]string `json:"enums"` // enum type OID to labels
}

// Snapshot returns the catalog's contents, ordered so that two runs against
// the same schema produce byte-identical output and a diff shows only what
// really changed.
func (c *Catalog) Snapshot() Snapshot {
	s := Snapshot{Enums: map[string][]string{}}

	for _, t := range c.types {
		s.Types = append(s.Types, t)
	}
	sort.Slice(s.Types, func(i, j int) bool { return s.Types[i].OID < s.Types[j].OID })

	s.Tables = c.Tables()

	for oid, labels := range c.enums {
		s.Enums[strconv.FormatUint(uint64(oid), 10)] = labels
	}
	return s
}

// FromSnapshot rebuilds a Catalog from recorded contents.
func FromSnapshot(s Snapshot) (*Catalog, error) {
	c := &Catalog{
		types:  map[uint32]Type{},
		tables: map[uint32]*Table{},
		enums:  map[uint32][]string{},
	}
	for _, t := range s.Types {
		if _, dup := c.types[t.OID]; dup {
			return nil, fmt.Errorf("catalog: type oid %d recorded twice", t.OID)
		}
		c.types[t.OID] = t
	}
	for _, t := range s.Tables {
		if _, dup := c.tables[t.OID]; dup {
			return nil, fmt.Errorf("catalog: table oid %d recorded twice", t.OID)
		}
		c.tables[t.OID] = t
	}
	for key, labels := range s.Enums {
		oid, err := strconv.ParseUint(key, 10, 32)
		if err != nil {
			return nil, fmt.Errorf("catalog: bad enum key %q: %w", key, err)
		}
		c.enums[uint32(oid)] = labels
	}
	return c, nil
}
