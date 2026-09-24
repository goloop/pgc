package migrate

import (
	"strings"
	"testing"
)

// FuzzSplitStatements checks the splitter never panics and never invents
// text: every statement it returns is a piece of the input.
func FuzzSplitStatements(f *testing.F) {
	for _, seed := range []string{
		"SELECT 1; SELECT 2",
		"CREATE FUNCTION f() RETURNS int LANGUAGE sql BEGIN ATOMIC SELECT 1; END;",
		"SELECT $$a;b$$; SELECT E'\\';'; /* x; /* y */ */ SELECT \"a;b\"",
		"CREATE OR REPLACE PROCEDURE p() BEGIN ATOMIC SELECT CASE WHEN true THEN 1 END; END",
		"SELECT a$b$c FROM t; -- done",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, sql string) {
		for _, stmt := range splitStatements(sql) {
			if !strings.Contains(sql, stmt) {
				t.Fatalf("statement %q is not part of the input", stmt)
			}
			transactionControl(stmt)
		}
	})
}
