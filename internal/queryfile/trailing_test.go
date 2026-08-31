package queryfile

import "testing"

// A comment that trails a finished statement (before the next header) must not
// end up inside the statement body.
func TestTrailingCommentIsNotPartOfBody(t *testing.T) {
	src := []byte("-- name: Schedulable :one\n" +
		"UPDATE tasks SET retry_count = retry_count + 1\n" +
		"WHERE id = $1\n" +
		"RETURNING retry_count;\n\n" +
		"-- A task is schedulable when its dependencies are done.\n")
	qs, err := ParseFile("q.sql", src)
	if err != nil {
		t.Fatal(err)
	}
	if len(qs) != 1 {
		t.Fatalf("got %d queries, want 1", len(qs))
	}
	if got := qs[0].SQL; contains(got, "schedulable") || contains(got, "--") {
		t.Errorf("trailing comment leaked into the body:\n%q", got)
	}
	if !contains(qs[0].SQL, "RETURNING retry_count") {
		t.Errorf("statement body was truncated: %q", qs[0].SQL)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
