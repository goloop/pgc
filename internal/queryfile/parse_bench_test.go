package queryfile

import (
	"strings"
	"testing"
)

func BenchmarkParseLongQuery(b *testing.B) {
	src := []byte("-- name: Long :one\nSELECT 1\n" + strings.Repeat(" + 1\n", 1000) + ";\n")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := ParseFile("query.sql", src); err != nil {
			b.Fatal(err)
		}
	}
}
