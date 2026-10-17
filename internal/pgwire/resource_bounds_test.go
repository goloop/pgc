package pgwire

import "testing"

func TestConnectTimeoutOverflow(t *testing.T) {
	if _, err := ParseURL("postgres://user@localhost/db?connect_timeout=9223372036854775807"); err == nil {
		t.Fatal("accepted overflowing timeout")
	}
}

func TestEmptyRowsConsumeResultBudget(t *testing.T) {
	for _, payload := range [][]byte{{0, 0}, {0, 1, 255, 255, 255, 255}, {0, 1, 0, 0, 0, 0}} {
		_, size, err := parseDataRow(payload)
		if err != nil {
			t.Fatal(err)
		}
		if size < 24 {
			t.Fatalf("empty row consumes %d bytes", size)
		}
	}
}
