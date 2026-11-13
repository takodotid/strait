package plugin

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// RoleCheckTimeout bounds one check of one address: the connection, TLS, authentication and the query.
const RoleCheckTimeout = time.Second

// RoleChecker asks a PostgreSQL server whether it is in recovery (RFC#0003, requirement 14).
type RoleChecker interface {
	InRecovery(ctx context.Context, address string, port int32) (bool, error)
}

// ErrNoCredentials means the controller runs with no role check credentials.
var ErrNoCredentials = errors.New("the controller has no role check credentials: set the chart value cnpg.confirmRole.secretName")

// PGRoleChecker runs SELECT pg_is_in_recovery() over TLS, with the user name and password in the files username and
// password of Dir. It reads them at each check, so that a new Secret takes effect without a restart. The role needs
// LOGIN and nothing else. It never writes.
type PGRoleChecker struct {
	Dir string
}

// InRecovery implements RoleChecker.
func (c PGRoleChecker) InRecovery(ctx context.Context, address string, port int32) (bool, error) {
	if c.Dir == "" {
		return false, ErrNoCredentials
	}
	user, err := os.ReadFile(filepath.Join(c.Dir, "username"))
	if err != nil {
		return false, fmt.Errorf("read the role check user name: %w", err)
	}
	password, err := os.ReadFile(filepath.Join(c.Dir, "password"))
	if err != nil {
		return false, fmt.Errorf("read the role check password: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, RoleCheckTimeout)
	defer cancel()
	// Only the host, the port and the TLS mode come from the string. sslmode=require encrypts and does not fall back
	// to plain text.
	cfg, err := pgconn.ParseConfig("dbname=postgres sslmode=require application_name=strait-role-check")
	if err != nil {
		return false, err
	}
	cfg.Host = address
	cfg.Port = uint16(port)
	cfg.User = strings.TrimSpace(string(user))
	cfg.Password = strings.TrimRight(string(password), "\r\n")
	cfg.ConnectTimeout = RoleCheckTimeout
	if cfg.TLSConfig != nil {
		cfg.TLSConfig.ServerName = address
	}
	cfg.Fallbacks = nil
	cfg.KerberosSrvName, cfg.KerberosSpn = "", ""
	conn, err := pgconn.ConnectConfig(ctx, cfg)
	if err != nil {
		return false, fmt.Errorf("connect to %s: %w", net.JoinHostPort(address, strconv.Itoa(int(port))), redact(err, cfg.Password))
	}
	defer conn.Close(context.WithoutCancel(ctx))
	res := conn.ExecParams(ctx, "SELECT pg_is_in_recovery()", nil, nil, nil, nil).Read()
	if res.Err != nil {
		return false, fmt.Errorf("query %s: %w", address, redact(res.Err, cfg.Password))
	}
	if len(res.Rows) != 1 || len(res.Rows[0]) != 1 {
		return false, fmt.Errorf("query %s: unexpected answer", address)
	}
	switch string(res.Rows[0][0]) {
	case "t":
		return true, nil
	case "f":
		return false, nil
	}
	return false, fmt.Errorf("query %s: unexpected answer %q", address, res.Rows[0][0])
}

// redact keeps the password out of an error message.
func redact(err error, password string) error {
	if password == "" || !strings.Contains(err.Error(), password) {
		return err
	}
	return errors.New(strings.ReplaceAll(err.Error(), password, "REDACTED"))
}
