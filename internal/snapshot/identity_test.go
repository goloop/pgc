package snapshot_test

import (
	"encoding/json"
	"strconv"
	"testing"

	"github.com/goloop/pgc/internal/catalog"
	"github.com/goloop/pgc/internal/pgwire"
	"github.com/goloop/pgc/internal/snapshot"
)

func identityFixture() *snapshot.File {
	return &snapshot.File{
		Queries: []snapshot.Query{{File: "queries/notes.sql", Name: "GetNote", SQL: "same SQL", Params: []uint32{300, 301, 302}, Columns: []pgwire.Column{
			{Name: "status", TypeOID: 300, TableOID: 100, Attnum: 1, TypeMod: -1},
			{Name: "amount", TypeOID: 302, TableOID: 100, Attnum: 2, TypeMod: -1},
		}}},
		Catalog: catalog.Snapshot{
			Types: []catalog.Type{
				{OID: 20, Schema: "pg_catalog", Name: "int8", Kind: 'b', Category: 'N'},
				{OID: 300, Schema: "public", Name: "status", Kind: 'e', Category: 'E'},
				{OID: 301, Schema: "public", Name: "_status", Kind: 'b', Category: 'A', Elem: 300},
				{OID: 302, Schema: "public", Name: "amount", Kind: 'd', Category: 'N', Base: 20},
				{OID: 500, Schema: "archive", Name: "status", Kind: 'e', Category: 'E'},
			},
			Tables: []*catalog.Table{{OID: 100, Schema: "public", Name: "notes", Columns: []catalog.Column{
				{Name: "status", Attnum: 1, TypeOID: 300, NotNull: true},
				{Name: "amount", Attnum: 2, TypeOID: 302, NotNull: true},
			}}},
			Enums: map[string][]string{"300": {"draft", "live"}, "500": {"closed"}},
		},
	}
}

func recreate(t *testing.T, f *snapshot.File) *snapshot.File {
	t.Helper()
	data, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	var next snapshot.File
	if err := json.Unmarshal(data, &next); err != nil {
		t.Fatal(err)
	}
	shift := func(oid uint32) uint32 {
		if oid >= 100 {
			return oid + 5000
		}
		return oid
	}
	for i := range next.Queries {
		q := &next.Queries[i]
		for j, oid := range q.Params {
			q.Params[j] = shift(oid)
		}
		for j := range q.Columns {
			c := &q.Columns[j]
			c.TypeOID, c.TableOID = shift(c.TypeOID), shift(c.TableOID)
		}
	}
	for i := range next.Catalog.Types {
		v := &next.Catalog.Types[i]
		v.OID, v.Elem, v.Base = shift(v.OID), shift(v.Elem), shift(v.Base)
	}
	for _, table := range next.Catalog.Tables {
		table.OID = shift(table.OID)
		for i := range table.Columns {
			table.Columns[i].TypeOID = shift(table.Columns[i].TypeOID)
		}
	}
	enums := map[string][]string{}
	for raw, labels := range next.Catalog.Enums {
		oid, _ := strconv.ParseUint(raw, 10, 32)
		enums[strconv.FormatUint(uint64(shift(uint32(oid))), 10)] = labels
	}
	next.Catalog.Enums = enums
	return &next
}

func TestCompareRecreatedDatabaseUsesSchemaIdentities(t *testing.T) {
	old := identityFixture()
	fresh := recreate(t, old)
	beforeOld, _ := json.Marshal(old)
	beforeFresh, _ := json.Marshal(fresh)
	if diffs := old.Compare(fresh); len(diffs) != 0 {
		t.Fatalf("identical schema with different OIDs: %v", diffs)
	}
	afterOld, _ := json.Marshal(old)
	afterFresh, _ := json.Marshal(fresh)
	if string(beforeOld) != string(afterOld) || string(beforeFresh) != string(afterFresh) {
		t.Fatal("comparison mutated an input")
	}
}

func TestCompareRecreatedDatabaseStillFindsSchemaChanges(t *testing.T) {
	changes := map[string]func(*snapshot.File){
		"table namespace":                     func(f *snapshot.File) { f.Catalog.Tables[0].Schema = "other" },
		"type namespace":                      func(f *snapshot.File) { f.Catalog.Types[1].Schema = "other" },
		"same type name in another namespace": func(f *snapshot.File) { f.Queries[0].Params[0] = 5500 },
		"enum labels":                         func(f *snapshot.File) { f.Catalog.Enums["5300"] = append(f.Catalog.Enums["5300"], "archived") },
		"domain base":                         func(f *snapshot.File) { f.Catalog.Types[3].Base = 5300 },
		"array element":                       func(f *snapshot.File) { f.Catalog.Types[2].Elem = 20 },
		"result modifier":                     func(f *snapshot.File) { f.Queries[0].Columns[0].TypeMod = 30 },
		"source column":                       func(f *snapshot.File) { f.Queries[0].Columns[0].Attnum = 2 },
		"nullability":                         func(f *snapshot.File) { f.Catalog.Tables[0].Columns[0].NotNull = false },
		"column type":                         func(f *snapshot.File) { f.Catalog.Tables[0].Columns[0].TypeOID = 20 },
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			old := identityFixture()
			fresh := recreate(t, old)
			change(fresh)
			if diffs := old.Compare(fresh); len(diffs) == 0 {
				t.Fatal("schema change was hidden by OID normalization")
			}
		})
	}
}
