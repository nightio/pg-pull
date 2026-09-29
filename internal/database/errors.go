package database

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"syscall"

	"github.com/nightio/pg-pull/internal/config"
	"github.com/jackc/pgx/v5/pgconn"
)

type connectionFailure struct {
	message string
	cause   error
}

func (e *connectionFailure) Error() string { return e.message }
func (e *connectionFailure) Unwrap() error { return e.cause }

func connectionError(db config.Database, role string, err error) error {
	reason := "Connection failed. Check the server address, port, network/VPN and PostgreSQL availability."
	var pgErr *pgconn.PgError
	var dnsErr *net.DNSError
	var netErr net.Error
	switch {
	case errors.Is(err, context.Canceled):
		reason = "Connection cancelled."
	// Prefer the server's final authentication response over a TLS fallback
	// failure: sslmode=prefer can produce both in the same error tree.
	case errors.As(err, &pgErr):
		switch pgErr.Code {
		case "28P01":
			reason = "Password authentication failed. Check the password and database user."
		case "28000":
			reason = "Access denied by PostgreSQL authentication rules. Check the user, pg_hba.conf and SSL requirements."
		case "42501":
			reason = "Access denied. Check that this user has CONNECT permission on the database."
		case "3D000":
			reason = "Database does not exist. Check dbname in the configuration."
		case "53300":
			reason = "PostgreSQL has no free connections. Close unused connections or reduce parallel_exports."
		default:
			reason = fmt.Sprintf("PostgreSQL rejected the connection (SQLSTATE %s). Check the server configuration and logs.", pgErr.Code)
		}
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &netErr) && netErr.Timeout():
		limit, _, _ := db.Timeouts()
		reason = fmt.Sprintf("Connection timed out (connect_timeout: %s). Check the network/VPN and server, or increase connect_timeout.", limit)
	case errors.As(err, &dnsErr):
		reason = "Cannot resolve the server hostname. Check host, DNS and VPN connectivity."
	case errors.Is(err, syscall.ECONNREFUSED):
		reason = "Connection refused. Check the port and whether PostgreSQL is running and accepting connections."
	case strings.Contains(strings.ToLower(err.Error()), "tls"), strings.Contains(strings.ToLower(err.Error()), "ssl"), strings.Contains(strings.ToLower(err.Error()), "x509"):
		reason = "TLS connection failed. Check sslmode, the server's TLS support, certificate trust and hostname."
	}
	// Do not render the driver's connection string or server-supplied details.
	return &connectionFailure{message: fmt.Sprintf("Cannot connect to %s %q at %s:%d as %q. %s", role, db.DBName, db.Host, db.Port, db.User, reason), cause: err}
}

// UserMessage preserves operation context while replacing known database errors
// with actionable guidance. The original error remains available to callers.
func UserMessage(err error) string {
	if err == nil {
		return ""
	}
	var connection *connectionFailure
	if errors.As(err, &connection) {
		return err.Error()
	}
	if errors.Is(err, context.Canceled) {
		return "Operation cancelled."
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		var message string
		switch pgErr.Code {
		case "55P03":
			message = "Could not acquire a database lock within lock_timeout. Another session may be using the table or sequence. Finish its transaction or increase lock_timeout, then retry."
		case "42501":
			message = "Permission denied. Check the database user's privileges for this operation (schema/table/sequence access; changing session_replication_role during restore also requires permission)."
		case "40P01":
			message = "Database deadlock detected. Finish competing transactions and retry."
		case "57014":
			message = "Database query cancelled. Check cancellation or server statement_timeout settings."
		case "0A000":
			if pgErr.Message == "cannot truncate a table referenced in a foreign key constraint" {
				message = "A foreign key prevents truncating the selected local tables."
				if pgErr.Detail != "" {
					message += " " + pgErr.Detail
				}
				message += " Create a new dump containing both the referencing and referenced tables, then restore it. The failed restore left local data unchanged."
			}
		}
		if message != "" {
			return strings.ReplaceAll(err.Error(), pgErr.Error(), message)
		}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "Database operation timed out. Check connectivity and configured time limits."
	}
	return err.Error()
}
