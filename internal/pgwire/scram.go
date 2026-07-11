package pgwire

import (
	"bytes"
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
)

// maxScramIterations bounds the PBKDF2 iteration count a server may request,
// so a hostile server cannot force an unbounded amount of CPU work. Real
// servers use about 4096; this ceiling is far above any legitimate value.
const maxScramIterations = 1 << 24

// scramClient runs the client side of one SCRAM-SHA-256 exchange (RFC 5802,
// RFC 7677) without channel binding, which is what PostgreSQL's SASL
// authentication speaks. PostgreSQL takes the user name from the startup
// message and ignores the one inside SCRAM, so user is normally empty.
type scramClient struct {
	user     string
	password string

	gs2             string // "n,," - no channel binding
	clientNonce     string
	clientFirstBare string
	serverSignature []byte
}

// newScramClient creates a client with a fresh random nonce.
func newScramClient(user, password string) (*scramClient, error) {
	var raw [18]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return nil, fmt.Errorf("pgwire: scram nonce: %w", err)
	}
	return newScramClientNonce(user, password,
		base64.StdEncoding.EncodeToString(raw[:])), nil
}

// newScramClientNonce is the deterministic constructor used by tests.
func newScramClientNonce(user, password, nonce string) *scramClient {
	return &scramClient{
		user:        user,
		password:    password,
		gs2:         "n,,",
		clientNonce: nonce,
	}
}

// clientFirst returns the client-first-message.
func (s *scramClient) clientFirst() []byte {
	s.clientFirstBare = "n=" + saslname(s.user) + ",r=" + s.clientNonce
	return []byte(s.gs2 + s.clientFirstBare)
}

// clientFinal consumes the server-first-message and returns the
// client-final-message carrying the proof. It also precomputes the expected
// server signature for verifyServer.
func (s *scramClient) clientFinal(serverFirst []byte) ([]byte, error) {
	serverNonce, salt, iters, err := parseServerFirst(string(serverFirst))
	if err != nil {
		return nil, err
	}
	// The server nonce must be our client nonce plus the server's own random
	// suffix; a nonce that merely equals ours (no suffix) is not acceptable.
	if !strings.HasPrefix(serverNonce, s.clientNonce) ||
		len(serverNonce) <= len(s.clientNonce) {
		return nil, fmt.Errorf("pgwire: scram: server nonce does not extend ours")
	}

	salted, err := pbkdf2.Key(sha256.New, s.password, salt, iters, sha256.Size)
	if err != nil {
		return nil, fmt.Errorf("pgwire: scram: %w", err)
	}
	clientKey := hmac256(salted, []byte("Client Key"))
	storedKey := sha256.Sum256(clientKey)

	withoutProof := "c=" +
		base64.StdEncoding.EncodeToString([]byte(s.gs2)) +
		",r=" + serverNonce
	authMessage := s.clientFirstBare + "," + string(serverFirst) + "," + withoutProof

	clientSig := hmac256(storedKey[:], []byte(authMessage))
	proof := make([]byte, len(clientKey))
	for i := range proof {
		proof[i] = clientKey[i] ^ clientSig[i]
	}

	serverKey := hmac256(salted, []byte("Server Key"))
	s.serverSignature = hmac256(serverKey, []byte(authMessage))

	final := withoutProof + ",p=" + base64.StdEncoding.EncodeToString(proof)
	return []byte(final), nil
}

// verifyServer checks the server-final-message signature, proving the server
// also knows the password derivative.
func (s *scramClient) verifyServer(serverFinal []byte) error {
	msg := string(serverFinal)
	if strings.HasPrefix(msg, "e=") {
		return fmt.Errorf("pgwire: scram: server error: %s", msg[2:])
	}
	if !strings.HasPrefix(msg, "v=") {
		return fmt.Errorf("pgwire: scram: malformed server-final message")
	}
	sig, err := base64.StdEncoding.DecodeString(
		strings.SplitN(msg[2:], ",", 2)[0])
	if err != nil {
		return fmt.Errorf("pgwire: scram: server signature: %w", err)
	}
	if !hmac.Equal(sig, s.serverSignature) {
		return fmt.Errorf("pgwire: scram: server signature mismatch")
	}
	return nil
}

// parseServerFirst extracts the nonce, salt and iteration count from
// "r=...,s=...,i=..." (any further extensions are ignored).
func parseServerFirst(msg string) (nonce string, salt []byte, iters int, err error) {
	for _, part := range strings.Split(msg, ",") {
		k, v, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		switch k {
		case "r":
			nonce = v
		case "s":
			salt, err = base64.StdEncoding.DecodeString(v)
			if err != nil {
				return "", nil, 0, fmt.Errorf("pgwire: scram salt: %w", err)
			}
		case "i":
			iters, err = strconv.Atoi(v)
			// Bound the iteration count: a hostile server could otherwise send
			// a huge value to burn CPU in PBKDF2 (real servers send ~4096).
			if err != nil || iters <= 0 || iters > maxScramIterations {
				return "", nil, 0, fmt.Errorf("pgwire: scram iterations %q", v)
			}
		}
	}
	if nonce == "" || len(salt) == 0 || iters == 0 {
		return "", nil, 0, fmt.Errorf("pgwire: scram: incomplete server-first message")
	}
	return nonce, salt, iters, nil
}

// saslname escapes the characters SCRAM reserves inside a user name.
func saslname(s string) string {
	if !strings.ContainsAny(s, "=,") {
		return s
	}
	var b bytes.Buffer
	for _, r := range s {
		switch r {
		case '=':
			b.WriteString("=3D")
		case ',':
			b.WriteString("=2C")
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// hmac256 is HMAC-SHA-256 of msg under key.
func hmac256(key, msg []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write(msg)
	return m.Sum(nil)
}
