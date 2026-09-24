package pgwire

import (
	"bufio"
	"context"
	"crypto/md5"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"slices"
	"time"
)

// opTimeout bounds every protocol round trip. pgc talks to a development
// database; anything slower than this is a hung connection, not a slow query.
const opTimeout = 30 * time.Second

// Conn is one authenticated connection to a PostgreSQL backend.
// It is not safe for concurrent use; pgc drives it from a single goroutine.
type Conn struct {
	conn net.Conn
	r    *bufio.Reader
	w    *bufio.Writer

	// opTimeout bounds one protocol round trip; 0 means no deadline.
	// Describe and catalog work keep the default; migrations disable it,
	// because DDL legitimately runs for as long as the server needs.
	opTimeout time.Duration

	// params holds the ParameterStatus values the server reported during
	// startup (server_version, client_encoding and friends).
	params map[string]string

	// cfg is kept for Cancel, which reaches the server on a connection of
	// its own; pid and secret are the BackendKeyData that identify this
	// session to it.
	cfg    Config
	pid    int32
	secret int32

	// secure is set when the connection runs over TLS.
	secure bool

	// txStatus is the transaction status of the last ReadyForQuery: 'I'
	// idle, 'T' in a transaction block, 'E' in a failed one.
	txStatus byte

	// broken is set once the connection can no longer be trusted to be in
	// step with the server; every later request returns it.
	broken error
}

// errClosed is what a request on a closed connection returns.
var errClosed = errors.New("pgwire: connection closed")

// usable returns why the connection cannot take another request, if it
// cannot.
func (c *Conn) usable() error {
	if c.broken != nil {
		return fmt.Errorf("pgwire: connection unusable after an earlier "+
			"failure: %w", c.broken)
	}
	return nil
}

// fail marks the connection broken and closes the socket. It is for errors
// after which an unknown part of the server's answer may still be on the
// wire - a timeout, a transport error, a message out of protocol - so the
// next request cannot be allowed to read it as its own.
func (c *Conn) fail(err error) error {
	if c.broken == nil {
		c.broken = err
		c.conn.Close()
	}
	return err
}

// ready records the transaction status a ReadyForQuery carries.
func (c *Conn) ready(payload []byte) error {
	if len(payload) != 1 {
		return c.fail(fmt.Errorf("pgwire: malformed ReadyForQuery"))
	}
	switch payload[0] {
	case 'I', 'T', 'E':
		c.txStatus = payload[0]
		return nil
	}
	return c.fail(fmt.Errorf("pgwire: unknown transaction status %q", payload[0]))
}

// TxStatus reports the transaction status after the last request: 'I' idle,
// 'T' inside a transaction block, 'E' inside a failed one.
func (c *Conn) TxStatus() byte { return c.txStatus }

// parameterStatus records a ParameterStatus message; the server sends one
// at startup and again whenever a reported setting changes.
func (c *Conn) parameterStatus(payload []byte) {
	r := &readBuf{b: payload}
	k, v := r.cstring(), r.cstring()
	if r.err == nil {
		c.params[k] = v
	}
}

// SetOpTimeout changes the per-operation deadline; 0 disables it entirely
// (dead peers are still caught by TCP keepalives).
func (c *Conn) SetOpTimeout(d time.Duration) {
	c.opTimeout = d
}

// Dial connects, negotiates TLS according to cfg.SSLMode, authenticates and
// waits for the server to become ready. The context bounds the whole startup
// sequence.
func Dial(ctx context.Context, cfg Config) (*Conn, error) {
	if cfg.ConnectTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, cfg.ConnectTimeout)
		defer cancel()
	}

	d := net.Dialer{}
	raw, err := d.DialContext(ctx, "tcp", cfg.addr())
	if err != nil {
		return nil, fmt.Errorf("pgwire: dial: %w", err)
	}

	deadline := time.Now().Add(opTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	raw.SetDeadline(deadline)

	// A cancelled context ends the startup at once, not at the deadline.
	stop := context.AfterFunc(ctx, func() { raw.SetDeadline(time.Now()) })
	defer stop()

	secured, err := negotiateTLS(raw, cfg)
	if err != nil {
		raw.Close()
		return nil, err
	}
	_, isTLS := secured.(*tls.Conn)
	raw = secured

	c := &Conn{
		conn:      raw,
		r:         bufio.NewReader(raw),
		w:         bufio.NewWriter(raw),
		opTimeout: opTimeout,
		params:    map[string]string{},
		cfg:       cfg,
		secure:    isTLS,
	}
	if err := c.startup(cfg); err != nil {
		raw.Close()
		if ctx.Err() != nil {
			return nil, fmt.Errorf("pgwire: startup: %w", ctx.Err())
		}
		return nil, err
	}
	if !stop() {
		raw.Close()
		return nil, fmt.Errorf("pgwire: startup: %w", ctx.Err())
	}
	raw.SetDeadline(time.Time{})
	return c, nil
}

// Parameter returns a ParameterStatus value reported by the server during
// startup, such as "server_version".
func (c *Conn) Parameter(name string) string {
	return c.params[name]
}

// closeTimeout bounds the goodbye: Close does not wait on a peer that has
// stopped reading.
var closeTimeout = 2 * time.Second

// Close sends Terminate and closes the connection. It returns within
// closeTimeout whatever the peer does.
func (c *Conn) Close() error {
	if c.broken != nil {
		return nil // fail already closed the socket
	}
	c.broken = errClosed
	c.conn.SetDeadline(time.Now().Add(closeTimeout))
	c.send('X', nil)
	c.w.Flush()
	return c.conn.Close()
}

// Cancel asks the server to abandon whatever this connection is running,
// the way an interrupted psql does: over a separate connection carrying the
// session's cancellation key. It may be called from another goroutine while
// a request is in flight; that request then returns the server's
// "canceling statement" error and the connection stays usable. Like every
// cancel request, it is advisory - the server may have finished already.
func (c *Conn) Cancel(ctx context.Context) error {
	if c.pid == 0 {
		return fmt.Errorf("pgwire: cancel: the server sent no cancellation key")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	d := net.Dialer{}
	raw, err := d.DialContext(ctx, "tcp", c.cfg.addr())
	if err != nil {
		return fmt.Errorf("pgwire: cancel: %w", err)
	}
	defer raw.Close()
	if dl, ok := ctx.Deadline(); ok {
		raw.SetDeadline(dl)
	}
	conn, err := negotiateTLS(raw, c.cfg)
	if err != nil {
		return fmt.Errorf("pgwire: cancel: %w", err)
	}
	var req [16]byte
	binary.BigEndian.PutUint32(req[0:], 16)
	binary.BigEndian.PutUint32(req[4:], 80877102) // CancelRequest code
	binary.BigEndian.PutUint32(req[8:], uint32(c.pid))
	binary.BigEndian.PutUint32(req[12:], uint32(c.secret))
	if _, err := conn.Write(req[:]); err != nil {
		return fmt.Errorf("pgwire: cancel: %w", err)
	}
	// The server answers by closing the connection; waiting for that makes
	// sure the request was read before Cancel returns.
	var one [1]byte
	conn.Read(one[:])
	return nil
}

// begin arms the per-operation deadline; done disarms it. A zero opTimeout
// leaves the connection without a deadline.
func (c *Conn) begin() {
	if c.opTimeout <= 0 {
		c.conn.SetDeadline(time.Time{})
		return
	}
	c.conn.SetDeadline(time.Now().Add(c.opTimeout))
}

func (c *Conn) done() { c.conn.SetDeadline(time.Time{}) }

// negotiateTLS performs the SSLRequest dance. With "disable" the connection
// stays plain. Otherwise the server is asked; "prefer" falls back to plain
// when the server declines, while "require" and "verify-full" insist.
func negotiateTLS(raw net.Conn, cfg Config) (net.Conn, error) {
	if cfg.SSLMode == "disable" {
		return raw, nil
	}

	var req [8]byte
	binary.BigEndian.PutUint32(req[:4], 8)
	binary.BigEndian.PutUint32(req[4:], 80877103) // SSLRequest code
	if _, err := raw.Write(req[:]); err != nil {
		return nil, fmt.Errorf("pgwire: ssl request: %w", err)
	}
	var answer [1]byte
	if _, err := raw.Read(answer[:]); err != nil {
		return nil, fmt.Errorf("pgwire: ssl response: %w", err)
	}

	switch answer[0] {
	case 'S':
		tcfg, err := buildTLSConfig(cfg)
		if err != nil {
			return nil, err
		}
		tconn := tls.Client(raw, tcfg)
		if err := tconn.Handshake(); err != nil {
			return nil, fmt.Errorf("pgwire: tls handshake: %w", err)
		}
		return tconn, nil
	case 'N':
		if cfg.SSLMode == "prefer" {
			return raw, nil
		}
		return nil, fmt.Errorf("pgwire: server refused TLS (sslmode=%s)", cfg.SSLMode)
	default:
		return nil, fmt.Errorf("pgwire: unexpected ssl answer %q", answer[0])
	}
}

// buildTLSConfig translates the sslmode and sslrootcert settings into a
// tls.Config:
//
//   - prefer/require encrypt without verifying the peer, unless a root
//     certificate is given - then they behave like verify-ca, matching what
//     users of the standard connection parameters expect;
//   - verify-ca checks the certificate chain but not the host name;
//   - verify-full checks both.
//
// A root certificate file replaces the system trust store - managed
// databases sign their server certificates with their own authority and
// hand out its PEM.
func buildTLSConfig(cfg Config) (*tls.Config, error) {
	mode := cfg.SSLMode
	var roots *x509.CertPool
	if cfg.SSLRootCert != "" {
		pem, err := os.ReadFile(cfg.SSLRootCert)
		if err != nil {
			return nil, fmt.Errorf("pgwire: sslrootcert: %w", err)
		}
		roots = x509.NewCertPool()
		if !roots.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf(
				"pgwire: sslrootcert: no certificates found in %s", cfg.SSLRootCert)
		}
		if mode == "prefer" || mode == "require" {
			mode = "verify-ca"
		}
	}

	switch mode {
	case "verify-full":
		return &tls.Config{ServerName: cfg.Host, RootCAs: roots}, nil
	case "verify-ca":
		// The chain is verified by hand because crypto/tls offers no
		// hostname-free verification mode.
		return &tls.Config{
			InsecureSkipVerify:    true,
			VerifyPeerCertificate: chainVerifier(roots),
		}, nil
	default: // prefer, require - encrypt only
		return &tls.Config{InsecureSkipVerify: true}, nil
	}
}

// chainVerifier checks the presented certificate chain against roots (or
// the system pool when roots is nil), ignoring the host name - the
// verify-ca contract.
func chainVerifier(roots *x509.CertPool) func([][]byte, [][]*x509.Certificate) error {
	return func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
		if len(rawCerts) == 0 {
			return fmt.Errorf("pgwire: server presented no certificate")
		}
		certs := make([]*x509.Certificate, 0, len(rawCerts))
		for _, raw := range rawCerts {
			cert, err := x509.ParseCertificate(raw)
			if err != nil {
				return fmt.Errorf("pgwire: server certificate: %w", err)
			}
			certs = append(certs, cert)
		}
		opts := x509.VerifyOptions{
			Roots:         roots,
			Intermediates: x509.NewCertPool(),
		}
		for _, cert := range certs[1:] {
			opts.Intermediates.AddCert(cert)
		}
		if _, err := certs[0].Verify(opts); err != nil {
			return fmt.Errorf("pgwire: verify-ca: %w", err)
		}
		return nil
	}
}

// startup sends the StartupMessage and runs the authentication conversation
// until the server reports ReadyForQuery.
func (c *Conn) startup(cfg Config) error {
	var body writeBuf
	body.int32(196608) // protocol 3.0
	body.cstring("user")
	body.cstring(cfg.User)
	body.cstring("database")
	body.cstring(cfg.Database)
	body.cstring("application_name")
	body.cstring("pgc")
	body.cstring("client_encoding")
	body.cstring("UTF8")
	body = append(body, 0)

	// The startup message has no type byte.
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(body)+4))
	if _, err := c.w.Write(hdr[:]); err != nil {
		return err
	}
	if _, err := c.w.Write(body); err != nil {
		return err
	}
	if err := c.w.Flush(); err != nil {
		return err
	}

	// auth tracks the authentication exchange, so a server - or whoever
	// sits between pgc and it - cannot skip a step: SCRAM is only complete
	// once the server has proved it knows the password too.
	var (
		scram     *scramClient
		scramStep int // 1 sent client-first, 2 sent client-final, 3 verified
		authDone  bool
	)
	for {
		typ, payload, err := c.recv()
		if err != nil {
			return fmt.Errorf("pgwire: startup: %w", err)
		}
		switch typ {
		case 'R': // Authentication*
			r := &readBuf{b: payload}
			code := r.int32()
			if r.err != nil {
				// A truncated 'R' payload leaves code at 0, which would be
				// misread as AuthenticationOk. Reject it instead.
				return fmt.Errorf("pgwire: truncated authentication message")
			}
			if authDone {
				return fmt.Errorf("pgwire: authentication message after " +
					"authentication completed")
			}
			switch code {
			case 0: // AuthenticationOk
				if scram != nil && scramStep != 3 {
					return fmt.Errorf("pgwire: the server ended SCRAM " +
						"authentication without proving it knows the password")
				}
				authDone = true
			case 3: // CleartextPassword
				if !c.secure && !isLoopback(cfg.Host) {
					return fmt.Errorf("pgwire: the server asked for the " +
						"password in clear text over an unencrypted " +
						"connection; use TLS (sslmode=require or stronger) " +
						"or SCRAM authentication")
				}
				var b writeBuf
				b.cstring(cfg.Password)
				if err := c.sendFlush('p', b); err != nil {
					return err
				}
			case 5: // MD5Password
				salt := r.bytes(4)
				if r.err != nil {
					return r.err
				}
				var b writeBuf
				b.cstring(md5Password(cfg.User, cfg.Password, salt))
				if err := c.sendFlush('p', b); err != nil {
					return err
				}
			case 10: // SASL: pick SCRAM-SHA-256
				if scram != nil {
					return fmt.Errorf("pgwire: SASL started twice")
				}
				var mechs []string
				for {
					m := r.cstring()
					if m == "" || r.err != nil {
						break
					}
					mechs = append(mechs, m)
				}
				if !slices.Contains(mechs, "SCRAM-SHA-256") {
					return fmt.Errorf(
						"pgwire: no common SASL mechanism (server offers %v)", mechs)
				}
				scram, err = newScramClient("", cfg.Password)
				if err != nil {
					return err
				}
				scramStep = 1
				first := scram.clientFirst()
				var b writeBuf
				b.cstring("SCRAM-SHA-256")
				b.int32(int32(len(first)))
				b.bytes(first)
				if err := c.sendFlush('p', b); err != nil {
					return err
				}
			case 11: // SASLContinue
				if scram == nil || scramStep != 1 {
					return fmt.Errorf("pgwire: SASL continue out of order")
				}
				scramStep = 2
				final, err := scram.clientFinal(payload[4:])
				if err != nil {
					return err
				}
				if err := c.sendFlush('p', final); err != nil {
					return err
				}
			case 12: // SASLFinal
				if scram == nil || scramStep != 2 {
					return fmt.Errorf("pgwire: SASL final out of order")
				}
				if err := scram.verifyServer(payload[4:]); err != nil {
					return err
				}
				scramStep = 3
			default:
				return fmt.Errorf("pgwire: unsupported authentication method %d", code)
			}
		case 'S': // ParameterStatus
			c.parameterStatus(payload)
		case 'K': // BackendKeyData - the key Cancel sends
			r := &readBuf{b: payload}
			c.pid, c.secret = r.int32(), r.int32()
			if r.err != nil {
				return r.err
			}
		case 'N': // NoticeResponse
		case 'E':
			return parseServerError(payload)
		case 'Z': // ReadyForQuery
			if !authDone {
				return fmt.Errorf("pgwire: server ready before authentication completed")
			}
			return c.ready(payload)
		default:
			return fmt.Errorf("pgwire: unexpected message %q during startup", typ)
		}
	}
}

// isLoopback reports whether host names this machine, where a password sent
// in clear text never crosses a network.
func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// sendFlush sends one message and flushes the write buffer.
func (c *Conn) sendFlush(typ byte, payload []byte) error {
	if err := c.send(typ, payload); err != nil {
		return err
	}
	return c.w.Flush()
}

// md5Password computes the legacy MD5 response:
// "md5" + hex(md5(hex(md5(password + user)) + salt)).
func md5Password(user, password string, salt []byte) string {
	inner := md5.Sum([]byte(password + user))
	hexInner := hex.EncodeToString(inner[:])
	outer := md5.Sum(append([]byte(hexInner), salt...))
	return "md5" + hex.EncodeToString(outer[:])
}
