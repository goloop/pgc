package pgwire

import (
	"bufio"
	"encoding/binary"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

// pipeConns returns a client Conn and the raw server side of an in-memory
// connection.
func pipeConns(t *testing.T) (*Conn, *Conn) {
	t.Helper()
	a, b := net.Pipe()
	t.Cleanup(func() { a.Close(); b.Close() })
	mk := func(c net.Conn) *Conn {
		return &Conn{
			conn: c, r: bufio.NewReader(c), w: bufio.NewWriter(c),
			opTimeout: time.Second, params: map[string]string{},
		}
	}
	return mk(a), mk(b)
}

// readStartup consumes the untyped StartupMessage on the server side.
func readStartup(s *Conn) error {
	var h [4]byte
	if _, err := io.ReadFull(s.r, h[:]); err != nil {
		return err
	}
	_, err := io.CopyN(io.Discard, s.r, int64(binary.BigEndian.Uint32(h[:]))-4)
	return err
}

// A server that offers SCRAM and then declares success without the challenge
// and the proof of its own knowledge of the password must not be believed.
func TestStartupRefusesAnUnfinishedSCRAM(t *testing.T) {
	c, s := pipeConns(t)
	go func() {
		s.conn.SetDeadline(time.Now().Add(time.Second))
		if readStartup(s) != nil {
			return
		}
		var auth writeBuf
		auth.int32(10)
		auth.cstring("SCRAM-SHA-256")
		auth.cstring("")
		s.sendFlush('R', auth)
		s.recv()
		var ok writeBuf
		ok.int32(0)
		s.send('R', ok)
		s.sendFlush('Z', []byte{'I'})
	}()
	err := c.startup(Config{User: "u", Password: "p", Database: "d"})
	if err == nil || !strings.Contains(err.Error(), "without proving") {
		t.Fatalf("err = %v, want the unfinished exchange refused", err)
	}
}

// ReadyForQuery before any AuthenticationOk is out of protocol.
func TestStartupRefusesReadyBeforeAuthentication(t *testing.T) {
	c, s := pipeConns(t)
	go func() {
		s.conn.SetDeadline(time.Now().Add(time.Second))
		if readStartup(s) != nil {
			return
		}
		s.sendFlush('Z', []byte{'I'})
	}()
	if err := c.startup(Config{User: "u", Database: "d"}); err == nil {
		t.Fatal("want an error for ReadyForQuery before authentication")
	}
}

// A clear-text password never crosses an unencrypted network.
func TestStartupRefusesCleartextOverPlainNetwork(t *testing.T) {
	c, s := pipeConns(t)
	go func() {
		s.conn.SetDeadline(time.Now().Add(time.Second))
		if readStartup(s) != nil {
			return
		}
		var auth writeBuf
		auth.int32(3)
		s.sendFlush('R', auth)
	}()
	err := c.startup(Config{Host: "db.example.com", User: "u", Password: "secret", Database: "d"})
	if err == nil || !strings.Contains(err.Error(), "clear text") {
		t.Fatalf("err = %v, want the clear-text request refused", err)
	}
}

// A server-final message is only meaningful after the exchange that computes
// the expected signature, and an empty signature never matches.
func TestScramServerFinalNeedsASignature(t *testing.T) {
	s := newScramClientNonce("", "pw", "nonce")
	if err := s.verifyServer([]byte("v=")); err == nil {
		t.Fatal("an empty signature before the exchange was accepted")
	}

	s = newScramClientNonce("user", "pencil", "rOprNGfwEbeRWgbNEkqO")
	s.clientFirst()
	if _, err := s.clientFinal([]byte("r=rOprNGfwEbeRWgbNEkqO%hvYDpWUa2RaTCAfuxFIlj)hNlF$k0," +
		"s=W22ZaJ0SNY7soEsUEjb6gQ==,i=4096")); err != nil {
		t.Fatal(err)
	}
	if err := s.verifyServer([]byte("v=")); err == nil {
		t.Fatal("an empty signature was accepted")
	}
}

func TestScramRefusesRepeatedAndMandatoryAttributes(t *testing.T) {
	for _, msg := range []string{
		"r=abcdef,s=c2FsdA==,i=4096,i=1",
		"r=abcdef,r=abcxyz,s=c2FsdA==,i=4096",
		"m=ext,r=abcdef,s=c2FsdA==,i=4096",
	} {
		if _, _, _, err := parseServerFirst(msg); err == nil {
			t.Errorf("parseServerFirst(%q): want error", msg)
		}
	}
}

func TestSASLPrep(t *testing.T) {
	cases := map[string]string{
		"pencil":       "pencil",    // ASCII is used as is
		"I­X":          "IX",        // soft hyphen is mapped to nothing
		"a b":          "a b",       // no-break space becomes a space
		"a​b":          "a b",       // zero width space is a space, as the server has it
		"пароль":       "пароль",    // already prepared
		"­":            "­",         // prepares to nothing: raw
		"x\u0007­y":    "x\u0007­y", // prohibited control character: raw
		"a b":         "a b",      // private use: raw
		"bad\xffutf8­": "bad\xffutf8­",
	}
	for in, want := range cases {
		if got := saslPrep(in); got != want {
			t.Errorf("saslPrep(%q) = %q, want %q", in, got, want)
		}
	}
}

// A URL whose parameters do not decode must fail, not lose them: a garbled
// sslmode=verify-full would otherwise become the default prefer.
func TestParseURLRefusesWhatItCannotHonour(t *testing.T) {
	for _, dsn := range []string{
		"postgres://u:p@localhost/d?sslmode=verify-full%ZZ",
		"postgres://u:p@localhost/d?channel_binding=require",
		"postgres://u:p@localhost/d?sslcert=client.pem&sslkey=client.key",
		"postgres://u:p@localhost/d?sslcrl=crl.pem",
		"postgres://u:p@localhost/d?gssencmode=require",
		"postgres://u:p@localhost/d?connect_timeout=soon",
	} {
		if _, err := ParseURL(dsn); err == nil {
			t.Errorf("ParseURL(%q): want error", dsn)
		}
	}

	cfg, err := ParseURL("postgres://u:p@localhost/d?connect_timeout=7&channel_binding=prefer&application_name=x")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ConnectTimeout != 7*time.Second {
		t.Errorf("ConnectTimeout = %v", cfg.ConnectTimeout)
	}
}

// No error of the parser may repeat the password.
func TestParseURLErrorsHideThePassword(t *testing.T) {
	for _, dsn := range []string{
		"postgres://audit:secret-marker@localhost:bad/db",
		"postgres://audit:secret-marker@localhost/db?sslmode=%ZZ",
		"postgres://audit:secret-marker@localhost/db?sslmode=magic",
		"mysql://audit:secret-marker@localhost/db",
		"postgres://audit:secret-marker@local host/db",
	} {
		_, err := ParseURL(dsn)
		if err == nil {
			t.Errorf("ParseURL(%q): want error", dsn)
			continue
		}
		if strings.Contains(err.Error(), "secret-marker") {
			t.Errorf("error repeats the password: %v", err)
		}
	}
}

// Only -1 marks a NULL field; any other negative length is out of protocol,
// and the connection must not be used after it.
func TestQueryRefusesANegativeFieldLength(t *testing.T) {
	c, s := pipeConns(t)
	go func() {
		s.recv()
		var b writeBuf
		b.int16(1)
		b.int32(-2)
		s.send('D', b)
		s.sendFlush('Z', []byte{'I'})
	}()
	if _, err := c.Query("SELECT 1"); err == nil {
		t.Fatal("want an error for field length -2")
	}
	if _, err := c.Query("SELECT 1"); err == nil ||
		!strings.Contains(err.Error(), "unusable") {
		t.Fatalf("err = %v, want the connection marked unusable", err)
	}
}

// After a timeout the answer to the timed-out request may still arrive; the
// connection is closed so no later request can take it for its own.
func TestTimeoutBreaksTheConnection(t *testing.T) {
	c, s := pipeConns(t)
	c.opTimeout = 30 * time.Millisecond
	go func() {
		s.recv()
		time.Sleep(100 * time.Millisecond)
		var b writeBuf
		b.int16(1)
		b.int32(2)
		b.bytes([]byte("42"))
		s.send('D', b)
		s.sendFlush('Z', []byte{'I'})
	}()
	if _, err := c.Query("SELECT pg_sleep(1)"); err == nil {
		t.Fatal("want a timeout")
	}
	c.opTimeout = time.Second
	if rows, err := c.Query("SELECT 99"); err == nil {
		t.Fatalf("a request after a timeout returned %v", rows)
	}
}

// COPY FROM STDIN makes the server wait for data; the client must answer with
// CopyFail instead of waiting for a ReadyForQuery that never comes.
func TestCopyInIsFailedNotAwaited(t *testing.T) {
	c, s := pipeConns(t)
	got := make(chan byte, 1)
	go func() {
		s.recv()
		s.sendFlush('G', []byte{0, 0, 0})
		typ, _, _ := s.recv()
		got <- typ
		s.send('E', []byte("SERROR\x00C57014\x00MCOPY from stdin failed\x00\x00"))
		s.sendFlush('Z', []byte{'I'})
	}()
	_, err := c.Query("COPY t FROM STDIN")
	if err == nil || !strings.Contains(err.Error(), "COPY") {
		t.Fatalf("err = %v, want the server's COPY error", err)
	}
	if typ := <-got; typ != 'f' {
		t.Fatalf("client answered %q, want CopyFail", typ)
	}
	if err := c.usable(); err != nil {
		t.Fatalf("the connection should stay usable: %v", err)
	}
}

// A message the client does not know leaves the rest of the answer unknown.
func TestUnknownMessageBreaksTheConnection(t *testing.T) {
	c, s := pipeConns(t)
	go func() {
		s.recv()
		s.sendFlush('?', nil)
	}()
	if _, err := c.Query("SELECT 1"); err == nil {
		t.Fatal("want an error for an unknown message")
	}
	if c.usable() == nil {
		t.Fatal("the connection should be unusable")
	}
}

// Exec reads the whole answer but keeps none of it.
func TestExecDiscardsRows(t *testing.T) {
	c, s := pipeConns(t)
	go func() {
		s.recv()
		for range 3 {
			var b writeBuf
			b.int16(1)
			b.int32(1)
			b.bytes([]byte("x"))
			s.send('D', b)
		}
		s.send('C', []byte("SELECT 3\x00"))
		s.sendFlush('Z', []byte{'T'})
	}()
	if err := c.Exec("SELECT 'x' FROM generate_series(1, 3)"); err != nil {
		t.Fatal(err)
	}
	if c.TxStatus() != 'T' {
		t.Fatalf("TxStatus = %q, want T", c.TxStatus())
	}
}

// Close must return even when the peer never reads the Terminate message.
func TestCloseDoesNotWaitOnADeadPeer(t *testing.T) {
	old := closeTimeout
	closeTimeout = 50 * time.Millisecond
	defer func() { closeTimeout = old }()

	c, _ := pipeConns(t)
	done := make(chan struct{})
	go func() { c.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Close blocked on a peer that does not read")
	}
	if _, err := c.Query("SELECT 1"); err == nil {
		t.Fatal("a closed connection accepted a request")
	}
}
