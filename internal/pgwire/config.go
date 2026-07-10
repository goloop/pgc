package pgwire

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

// Config holds everything needed to reach one PostgreSQL server. Build it
// with ParseURL; the zero value is not usable.
type Config struct {
	Host     string // host name or IP, defaults to localhost
	Port     string // TCP port, defaults to 5432
	User     string
	Password string
	Database string // defaults to the user name, like the server does
	SSLMode  string // disable, prefer (default), require or verify-full
}

// ParseURL parses a postgres:// (or postgresql://) connection URL of the
// usual shape:
//
//	postgres://user:password@host:5432/dbname?sslmode=disable
//
// Unknown query parameters are ignored so URLs written for other tools keep
// working. The sslmode values understood are disable, prefer (the default),
// require and verify-full.
func ParseURL(dsn string) (Config, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return Config{}, fmt.Errorf("pgwire: parse url: %w", err)
	}
	if u.Scheme != "postgres" && u.Scheme != "postgresql" {
		return Config{}, fmt.Errorf(
			"pgwire: unsupported scheme %q (want postgres://)", u.Scheme)
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
	if m := u.Query().Get("sslmode"); m != "" {
		switch m {
		case "disable", "prefer", "require", "verify-full":
			cfg.SSLMode = m
		default:
			return Config{}, fmt.Errorf("pgwire: unsupported sslmode %q", m)
		}
	}
	if cfg.User == "" {
		return Config{}, fmt.Errorf("pgwire: user is required in the url")
	}
	return cfg, nil
}

// addr returns the host:port dial target.
func (c Config) addr() string {
	return net.JoinHostPort(c.Host, c.Port)
}
