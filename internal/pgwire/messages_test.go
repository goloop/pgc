package pgwire

import (
	"bufio"
	"bytes"
	"net"
	"testing"
	"time"
)

func TestSendRecvRoundTrip(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	c := &Conn{conn: client, r: bufio.NewReader(client), w: bufio.NewWriter(client)}
	s := &Conn{conn: server, r: bufio.NewReader(server), w: bufio.NewWriter(server)}

	var b writeBuf
	b.int32(42)
	b.int16(7)
	b.cstring("hello")

	go func() {
		c.send('X', b)
		c.w.Flush()
	}()

	server.SetDeadline(time.Now().Add(time.Second))
	typ, payload, err := s.recv()
	if err != nil {
		t.Fatal(err)
	}
	if typ != 'X' {
		t.Errorf("type = %q", typ)
	}
	r := &readBuf{b: payload}
	if v := r.int32(); v != 42 {
		t.Errorf("int32 = %d", v)
	}
	if v := r.int16(); v != 7 {
		t.Errorf("int16 = %d", v)
	}
	if v := r.cstring(); v != "hello" {
		t.Errorf("cstring = %q", v)
	}
	if r.err != nil {
		t.Fatal(r.err)
	}
}

func TestReadBufShortPayload(t *testing.T) {
	r := &readBuf{b: []byte{0x01}}
	r.int32()
	if r.err == nil {
		t.Error("int32 on 1 byte: want error")
	}

	r = &readBuf{b: []byte("no-terminator")}
	r.cstring()
	if r.err == nil {
		t.Error("cstring without NUL: want error")
	}

	r = &readBuf{b: []byte{1, 2}}
	r.bytes(5)
	if r.err == nil {
		t.Error("bytes(5) of 2: want error")
	}
}

func TestRecvRejectsHugeMessage(t *testing.T) {
	var raw bytes.Buffer
	raw.Write([]byte{'D', 0xFF, 0xFF, 0xFF, 0xFF})
	c := &Conn{r: bufio.NewReader(&raw)}
	if _, _, err := c.recv(); err == nil {
		t.Error("want error for oversized message")
	}
}

func TestParseServerErrorFields(t *testing.T) {
	var p []byte
	for _, f := range []struct {
		k byte
		v string
	}{{'S', "ERROR"}, {'C', "42P01"}, {'M', "relation does not exist"}, {'P', "15"}} {
		p = append(p, f.k)
		p = append(p, f.v...)
		p = append(p, 0)
	}
	p = append(p, 0)

	e := parseServerError(p)
	if e.Severity != "ERROR" || e.Code != "42P01" || e.Position != "15" {
		t.Fatalf("parsed = %+v", e)
	}
	if got := e.Error(); got == "" {
		t.Error("empty Error()")
	}
}
