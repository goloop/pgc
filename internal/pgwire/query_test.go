package pgwire

import (
	"bufio"
	"net"
	"testing"
	"time"
)

// A DataRow announcing a negative field count (0xffff read as int16) must
// return an error, not panic in make([]Value, 0, n).
func TestQueryRejectsNegativeFieldCount(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	c := &Conn{conn: client, r: bufio.NewReader(client), w: bufio.NewWriter(client)}
	s := &Conn{conn: server, r: bufio.NewReader(server), w: bufio.NewWriter(server)}

	go func() {
		server.SetDeadline(time.Now().Add(time.Second))
		if _, _, err := s.recv(); err != nil { // consume the Query message
			return
		}
		var d writeBuf
		d.int16(-1) // 0xffff field count
		s.send('D', d)
		s.w.Flush()
	}()

	client.SetDeadline(time.Now().Add(time.Second))
	_, err := c.Query("SELECT 1")
	if err == nil {
		t.Fatal("want error for a DataRow with a negative field count")
	}
}
