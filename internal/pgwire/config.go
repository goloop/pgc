package pgwire

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Config holds everything needed to reach one PostgreSQL server. Build it
// with ParseURL; the zero value is not usable.
type Config struct {
	Host     string // host name or IP, defaults to localhost
	Port     string // TCP port, defaults to 5432
	User     string
	Password string
	Database string // defaults to the user name, like the server does

	// SSLMode is disable, prefer (default), require, verify-ca or
	// verify-full.
	SSLMode string

	// SSLRootCert is a path to a PEM file with the certificate authority
	// to trust instead of the system roots - the usual arrangement for
	// managed databases, which hand out their own CA file.
	SSLRootCert string

	// ConnectTimeout bounds connecting and authenticating; zero leaves it to
	// the context given to Dial.
	ConnectTimeout time.Duration
}

// ParseURL parses a postgres:// (or postgresql://) connection URL of the
// usual shape:
//
//	postgres://user:password@host:5432/dbname?sslmode=disable
//
// The query parameters understood are sslmode (disable, prefer - the
// default -, require, verify-ca, verify-full), sslrootcert and
// connect_timeout. A parameter that asks for a protection pgc does not
// provide - client certificates, channel_binding=require, a revocation list,
// GSS encryption - is an error rather than ignored: a connection must not
// look safer than it is. Other parameters, written for other tools
// (application_name and the like), are ignored.
//
// Errors never repeat the URL, which carries the password.
func ParseURL(dsn string) (Config, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		// A *url.Error quotes the whole URL, password included; keep only
		// the reason.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return Config{}, fmt.Errorf("pgwire: parse url: %w", err)
	}
	if u.Scheme != "postgres" && u.Scheme != "postgresql" {
		return Config{}, fmt.Errorf(
			"pgwire: unsupported scheme %q (want postgres://)", u.Scheme)
	}
	query, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		// A parameter that does not decode would otherwise vanish, and a
		// garbled sslmode=verify-full would quietly become prefer.
		return Config{}, fmt.Errorf("pgwire: url parameters: %w", err)
	}

	cfg := Config{
		Host:    "localhost",
		Port:    "5432",
		SSLMode: "prefer",
	}
	if h := u.Hostname(); h != "" {
		cfg.Host = h
	}
	if p := u.Port(); p != "" {
		cfg.Port = p
	}
	if u.User != nil {
		cfg.User = u.User.Username()
		cfg.Password, _ = u.User.Password()
	}
	if db := strings.TrimPrefix(u.Path, "/"); db != "" {
		cfg.Database = db
	}
	if cfg.Database == "" {
		cfg.Database = cfg.User
	}
	if m := query.Get("sslmode"); m != "" {
		switch m {
		case "disable", "prefer", "require", "verify-ca", "verify-full":
			cfg.SSLMode = m
		default:
			return Config{}, fmt.Errorf("pgwire: unsupported sslmode %q", m)
		}
	}
	cfg.SSLRootCert = query.Get("sslrootcert")
	if v := query.Get("connect_timeout"); v != "" {
		secs, err := strconv.Atoi(v)
		if err != nil || secs < 0 {
			return Config{}, fmt.Errorf("pgwire: connect_timeout %q is not "+
				"a number of seconds", v)
		}
		cfg.ConnectTimeout = time.Duration(secs) * time.Second
	}
	if err := refuseUnsupported(query); err != nil {
		return Config{}, err
	}
	if cfg.User == "" {
		return Config{}, fmt.Errorf("pgwire: user is required in the url")
	}
	return cfg, nil
}

// refuseUnsupported rejects the parameters that ask for a protection pgc
// cannot give. Ignoring them would connect anyway, with less security than
// the URL promises.
func refuseUnsupported(query url.Values) error {
	for _, name := range []string{
		"sslcert", "sslkey", "sslpassword", "sslcrl", "sslcrldir",
		"require_auth",
	} {
		if query.Has(name) {
			return fmt.Errorf("pgwire: %s is not supported", name)
		}
	}
	switch v := query.Get("channel_binding"); v {
	case "", "disable", "prefer":
	default:
		return fmt.Errorf("pgwire: channel_binding=%s is not supported", v)
	}
	switch v := query.Get("gssencmode"); v {
	case "", "disable", "prefer":
	default:
		return fmt.Errorf("pgwire: gssencmode=%s is not supported", v)
	}
	return nil
}

// addr returns the host:port dial target.
func (c Config) addr() string {
	return net.JoinHostPort(c.Host, c.Port)
}
