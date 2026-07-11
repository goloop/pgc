package pgwire

import (
	"strings"
	"testing"
)

// The SCRAM-SHA-256 test vector from RFC 7677, section 3.
func TestScramRFC7677Vector(t *testing.T) {
	s := newScramClientNonce("user", "pencil", "rOprNGfwEbeRWgbNEkqO")

	first := string(s.clientFirst())
	if first != "n,,n=user,r=rOprNGfwEbeRWgbNEkqO" {
		t.Fatalf("client-first = %q", first)
	}

	serverFirst := "r=rOprNGfwEbeRWgbNEkqO%hvYDpWUa2RaTCAfuxFIlj)hNlF$k0," +
		"s=W22ZaJ0SNY7soEsUEjb6gQ==,i=4096"
	final, err := s.clientFinal([]byte(serverFirst))
	if err != nil {
		t.Fatal(err)
	}
	want := "c=biws,r=rOprNGfwEbeRWgbNEkqO%hvYDpWUa2RaTCAfuxFIlj)hNlF$k0," +
		"p=dHzbZapWIk4jUhN+Ute9ytag9zjfMHgsqmmiz7AndVQ="
	if string(final) != want {
		t.Fatalf("client-final:\n got %q\nwant %q", final, want)
	}

	serverFinal := "v=6rriTRBi23WpRR/wtup+mMhUZUn/dB5nLTJRsjl95G4="
	if err := s.verifyServer([]byte(serverFinal)); err != nil {
		t.Fatalf("verifyServer: %v", err)
	}
}

func TestScramRejectsForeignNonce(t *testing.T) {
	s := newScramClientNonce("", "pw", "abc")
	s.clientFirst()
	_, err := s.clientFinal([]byte("r=zzz,s=c2FsdA==,i=4096"))
	if err == nil || !strings.Contains(err.Error(), "nonce") {
		t.Fatalf("want nonce error, got %v", err)
	}
}

// A server nonce that only equals the client nonce (adds no suffix of its own)
// must be rejected.
func TestScramRejectsEqualNonce(t *testing.T) {
	s := newScramClientNonce("", "pw", "abc")
	s.clientFirst()
	_, err := s.clientFinal([]byte("r=abc,s=c2FsdA==,i=4096"))
	if err == nil || !strings.Contains(err.Error(), "nonce") {
		t.Fatalf("want nonce error for equal nonce, got %v", err)
	}
}

// An absurd iteration count from a hostile server must be rejected, not run
// through PBKDF2.
func TestScramRejectsHugeIterations(t *testing.T) {
	if _, _, _, err := parseServerFirst("r=abc,s=c2FsdA==,i=99999999"); err == nil {
		t.Fatal("want error for an iteration count above the ceiling")
	}
	// A normal count is still accepted.
	if _, _, _, err := parseServerFirst("r=abc,s=c2FsdA==,i=4096"); err != nil {
		t.Fatalf("normal iteration count rejected: %v", err)
	}
}

func TestScramServerErrorAndBadSignature(t *testing.T) {
	s := newScramClientNonce("", "pw", "abc")
	s.clientFirst()
	if _, err := s.clientFinal([]byte("r=abcdef,s=c2FsdA==,i=4096")); err != nil {
		t.Fatal(err)
	}
	if err := s.verifyServer([]byte("e=other-error")); err == nil {
		t.Error("want error for e= response")
	}
	if err := s.verifyServer([]byte("v=AAAA")); err == nil {
		t.Error("want error for wrong signature")
	}
}

func TestParseServerFirstIncomplete(t *testing.T) {
	for _, msg := range []string{"", "r=abc", "r=abc,s=c2FsdA==", "r=abc,s=!!,i=1",
		"r=abc,s=c2FsdA==,i=zero"} {
		if _, _, _, err := parseServerFirst(msg); err == nil {
			t.Errorf("parseServerFirst(%q): want error", msg)
		}
	}
}

func TestSaslname(t *testing.T) {
	if got := saslname("a=b,c"); got != "a=3Db=2Cc" {
		t.Errorf("saslname = %q", got)
	}
	if got := saslname("plain"); got != "plain" {
		t.Errorf("saslname = %q", got)
	}
}
