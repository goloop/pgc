package pgwire

import (
	"bufio"
	"encoding/binary"
	"net"
	"strings"
	"testing"
	"time"
)

// slowBackend answers one simple Query with EmptyQueryResponse+ReadyForQuery
// after a delay, imitating a long-running statement.
func slowBackend(t *testing.T, server net.Conn, delay time.Duration) {
	t.Helper()
	go func() {
		r := bufio.NewReader(server)
		var hdr [5]byte
		if _, err := readFull(r, hdr[:]); err != nil {
			return
		}
		n := binary.BigEndian.Uint32(hdr[1:])
		payload := make([]byte, n-4)
		readFull(r, payload)

		time.Sleep(delay)
		server.Write([]byte{'I', 0, 0, 0, 4})
		server.Write([]byte{'Z', 0, 0, 0, 5, 'I'})
	}()
}

func readFull(r *bufio.Reader, p []byte) (int, error) {
	total := 0
	for total < len(p) {
		n, err := r.Read(p[total:])
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

// A statement slower than the op timeout must fail; with the timeout
// disabled the same statement must be waited out - migrations depend on it.
func TestOpTimeoutBounds(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	c := &Conn{
		conn: client, r: bufio.NewReader(client), w: bufio.NewWriter(client),
		opTimeout: 50 * time.Millisecond,
	}
	slowBackend(t, server, 200*time.Millisecond)

	if _, err := c.Query("SELECT pg_sleep(1)"); err == nil ||
		!strings.Contains(err.Error(), "timeout") {
		t.Fatalf("want timeout error, got %v", err)
	}
}

func TestOpTimeoutDisabled(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	c := &Conn{
		conn: client, r: bufio.NewReader(client), w: bufio.NewWriter(client),
		opTimeout: 50 * time.Millisecond,
	}
	c.SetOpTimeout(0)
	slowBackend(t, server, 200*time.Millisecond)

	rows, err := c.Query("CREATE INDEX CONCURRENTLY big ON t (x)")
	if err != nil {
		t.Fatalf("disabled timeout must wait the statement out: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("rows = %v", rows)
	}
}
