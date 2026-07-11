package pgwire

import (
	"bufio"
	"context"
	"crypto/md5"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"encoding/hex"
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

	secured, err := negotiateTLS(raw, cfg)
	if err != nil {
		raw.Close()
		return nil, err
	}
	raw = secured

	c := &Conn{
		conn:      raw,
		r:         bufio.NewReader(raw),
		w:         bufio.NewWriter(raw),
		opTimeout: opTimeout,
		params:    map[string]string{},
	}
	if err := c.startup(cfg); err != nil {
		raw.Close()
		return nil, err
	}
	raw.SetDeadline(time.Time{})
	return c, nil
}

// Parameter returns a ParameterStatus value reported by the server during
// startup, such as "server_version".
func (c *Conn) Parameter(name string) string {
	return c.params[name]
}

// Close sends Terminate and closes the connection.
func (c *Conn) Close() error {
	c.send('X', nil)
	c.w.Flush()
	return c.conn.Close()
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

	var scram *scramClient
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
			switch code {
			case 0: // AuthenticationOk
			case 3: // CleartextPassword
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
				first := scram.clientFirst()
				var b writeBuf
				b.cstring("SCRAM-SHA-256")
				b.int32(int32(len(first)))
				b.bytes(first)
				if err := c.sendFlush('p', b); err != nil {
					return err
				}
			case 11: // SASLContinue
				if scram == nil {
					return fmt.Errorf("pgwire: SASL continue before SASL start")
				}
				final, err := scram.clientFinal(payload[4:])
				if err != nil {
					return err
				}
				if err := c.sendFlush('p', final); err != nil {
					return err
				}
			case 12: // SASLFinal
				if scram == nil {
					return fmt.Errorf("pgwire: SASL final before SASL start")
				}
				if err := scram.verifyServer(payload[4:]); err != nil {
					return err
				}
			default:
				return fmt.Errorf("pgwire: unsupported authentication method %d", code)
			}
		case 'S': // ParameterStatus
			r := &readBuf{b: payload}
			k, v := r.cstring(), r.cstring()
			if r.err == nil {
				c.params[k] = v
			}
		case 'K': // BackendKeyData - cancellation keys, unused here
		case 'N': // NoticeResponse
		case 'E':
			return parseServerError(payload)
		case 'Z': // ReadyForQuery
			return nil
		default:
			return fmt.Errorf("pgwire: unexpected message %q during startup", typ)
		}
	}
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
