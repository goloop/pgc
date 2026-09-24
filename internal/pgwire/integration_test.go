//go:build integration

package pgwire

import (
	"context"
	"errors"
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

// dialIntegration connects to the server named by PGC_DATABASE_URL.
func dialIntegration(t *testing.T) (*Conn, Config) {
	t.Helper()
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
	t.Cleanup(func() { conn.Close() })
	return conn, cfg
}

// A request after a timeout must never read the timed-out request's answer.
func TestIntegrationTimeoutNeverDesynchronizes(t *testing.T) {
	conn, _ := dialIntegration(t)
	conn.SetOpTimeout(25 * time.Millisecond)
	if _, err := conn.Query("SELECT pg_sleep(0.12)"); err == nil {
		t.Fatal("want a timeout")
	}
	conn.SetOpTimeout(time.Second)
	if rows, err := conn.Query("SELECT 99"); err == nil {
		t.Fatalf("a request after a timeout returned %v", rows)
	}
}

// COPY FROM STDIN is refused at once and leaves the connection usable.
func TestIntegrationCopyIn(t *testing.T) {
	conn, _ := dialIntegration(t)
	if err := conn.Exec("CREATE TEMP TABLE copy_target(n int)"); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := conn.Exec("COPY copy_target FROM STDIN"); err == nil {
		t.Fatal("want an error")
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("COPY took %v to fail", d)
	}
	rows, err := conn.Query("SELECT 7")
	if err != nil || rows[0][0].S != "7" {
		t.Fatalf("connection unusable after COPY: %v %v", rows, err)
	}
	// COPY TO STDOUT data is drained and discarded.
	if err := conn.Exec("COPY (SELECT generate_series(1, 1000)) TO STDOUT"); err != nil {
		t.Fatal(err)
	}
}

// A result larger than maxResult is refused, and the connection stays in step.
func TestIntegrationResultBudget(t *testing.T) {
	conn, _ := dialIntegration(t)
	if _, err := conn.Query(
		"SELECT repeat('x', 1 << 20) FROM generate_series(1, 80)"); err == nil {
		t.Fatal("an 80 MiB result was held in memory")
	}
	if err := conn.Exec(
		"SELECT repeat('x', 1 << 20) FROM generate_series(1, 80)"); err != nil {
		t.Fatalf("Exec should discard the rows: %v", err)
	}
	rows, err := conn.Query("SELECT 8")
	if err != nil || rows[0][0].S != "8" {
		t.Fatalf("connection unusable after the budget: %v %v", rows, err)
	}
}

// Cancel stops a running statement and leaves the connection usable.
func TestIntegrationCancel(t *testing.T) {
	conn, _ := dialIntegration(t)
	conn.SetOpTimeout(0)
	go func() {
		time.Sleep(100 * time.Millisecond)
		if err := conn.Cancel(context.Background()); err != nil {
			t.Error(err)
		}
	}()
	start := time.Now()
	err := conn.Exec("SELECT pg_sleep(20)")
	var se *ServerError
	if !errors.As(err, &se) || se.Code != "57014" {
		t.Fatalf("err = %v, want query_canceled", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("cancel took %v", d)
	}
	if err := conn.Exec("SELECT 1"); err != nil {
		t.Fatalf("connection unusable after cancel: %v", err)
	}
}

// A password the server prepares with SASLprep authenticates as typed.
func TestIntegrationSASLPrepPassword(t *testing.T) {
	conn, cfg := dialIntegration(t)
	const role = "pgc_saslprep_probe"
	conn.Exec("DROP ROLE IF EXISTS " + role)
	if err := conn.Exec("CREATE ROLE " + role +
		" LOGIN PASSWORD 'I­X y'"); err != nil {
		t.Skipf("cannot create a role: %v", err)
	}
	t.Cleanup(func() { conn.Exec("DROP ROLE IF EXISTS " + role) })

	cfg.User, cfg.Password = role, "I­X y"
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	probe, err := Dial(ctx, cfg)
	if err != nil {
		t.Fatalf("the password as typed did not authenticate: %v", err)
	}
	probe.Close()
}

// The transaction status follows the session.
func TestIntegrationTxStatus(t *testing.T) {
	conn, _ := dialIntegration(t)
	if conn.TxStatus() != 'I' {
		t.Fatalf("after startup: %q", conn.TxStatus())
	}
	conn.Exec("BEGIN")
	if conn.TxStatus() != 'T' {
		t.Fatalf("after BEGIN: %q", conn.TxStatus())
	}
	conn.Exec("SELECT 1/0")
	if conn.TxStatus() != 'E' {
		t.Fatalf("after an error: %q", conn.TxStatus())
	}
	conn.Exec("ROLLBACK")
	if conn.TxStatus() != 'I' {
		t.Fatalf("after ROLLBACK: %q", conn.TxStatus())
	}
}
