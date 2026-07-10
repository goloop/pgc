//go:build integration

package pgwire

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestIntegrationDescribe runs the full startup + describe conversation
// against a live server named by PGC_DATABASE_URL. It assumes nothing about
// the schema: pg_catalog is always there.
func TestIntegrationDescribe(t *testing.T) {
	dsn := os.Getenv("PGC_DATABASE_URL")
	if dsn == "" {
		t.Skip("PGC_DATABASE_URL is not set")
	}
	cfg, err := ParseURL(dsn)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := Dial(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	if v := conn.Parameter("server_version"); v == "" {
		t.Error("no server_version parameter after startup")
	}

	st, err := conn.Describe(
		"SELECT typname, typtype FROM pg_catalog.pg_type WHERE oid = $1")
	if err != nil {
		t.Fatal(err)
	}
	if len(st.ParamOIDs) != 1 {
		t.Fatalf("params = %v", st.ParamOIDs)
	}
	if len(st.Columns) != 2 || st.Columns[0].Name != "typname" {
		t.Fatalf("columns = %+v", st.Columns)
	}
	if st.Columns[0].TableOID == 0 {
		t.Error("typname should have a table origin")
	}

	rows, err := conn.Query("SELECT 1, NULL")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || !rows[0][0].Valid || rows[0][0].S != "1" || rows[0][1].Valid {
		t.Fatalf("rows = %+v", rows)
	}

	// A broken statement must come back as a *ServerError with a position,
	// not tear down the connection.
	if _, err := conn.Describe("SELECT no_such_column"); err == nil {
		t.Fatal("want server error")
	}
	if _, err := conn.Describe("SELECT 1"); err != nil {
		t.Fatalf("connection unusable after server error: %v", err)
	}
}
