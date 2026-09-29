package database

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strconv"

	"github.com/nightio/pg-pull/internal/config"
	"github.com/jackc/pgx/v5"
)

type Column struct {
	Name      string
	Type      string
	Generated bool
}

type SequenceRef struct {
	Name             string
	OwnerTable       string
	OwnerColumn      string
	ReferencedTables []string
}

func parseConnection(db config.Database, password string, readOnly bool) (*pgx.ConnConfig, error) {
	connectTimeout, lockTimeout, err := db.Timeouts()
	if err != nil {
		return nil, err
	}
	u := &url.URL{
		Scheme: "postgres",
		User:   url.UserPassword(db.User, password),
		Host:   net.JoinHostPort(db.Host, strconv.Itoa(int(db.Port))),
		Path:   db.DBName,
	}
	query := u.Query()
	if db.SSLMode != "" {
		query.Set("sslmode", db.SSLMode)
	}
	u.RawQuery = query.Encode()
	parsed, err := pgx.ParseConfig(u.String())
	if err != nil {
		// pgx ParseConfigError includes the connection URL, including its password.
		return nil, fmt.Errorf("invalid PostgreSQL connection settings; check sslmode and PostgreSQL environment variables")
	}
	parsed.ConnectTimeout = connectTimeout
	parsed.RuntimeParams["lock_timeout"] = strconv.FormatInt(lockTimeout.Milliseconds(), 10)
	parsed.RuntimeParams["application_name"] = "pg-pull"
	parsed.RuntimeParams["client_encoding"] = "UTF8"
	if readOnly {
		// This server-side default is the primary safety barrier. Any transaction
		// opened through this connection rejects writes, even if the source role
		// itself has write privileges.
		parsed.RuntimeParams["default_transaction_read_only"] = "on"
	}
	return parsed, nil
}

func quoteIdentifier(parts ...string) string {
	return pgx.Identifier(parts).Sanitize()
}

func columnNames(columns []Column) []string {
	names := make([]string, 0, len(columns))
	for _, column := range columns {
		if !column.Generated {
			names = append(names, column.Name)
		}
	}
	return names
}

func quotedColumns(columns []string) string {
	result := ""
	for i, column := range columns {
		if i > 0 {
			result += ", "
		}
		result += quoteIdentifier(column)
	}
	return result
}

func queryColumns(ctx context.Context, q interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}, schema, table string) ([]Column, error) {
	rows, err := q.Query(ctx, `
SELECT a.attname, pg_catalog.format_type(a.atttypid, a.atttypmod), a.attgenerated <> ''
FROM pg_catalog.pg_attribute a
JOIN pg_catalog.pg_class c ON c.oid = a.attrelid
JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname = $1
  AND c.relname = $2
  AND c.relkind IN ('r', 'p')
  AND a.attnum > 0
  AND NOT a.attisdropped
ORDER BY a.attnum`, schema, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var columns []Column
	for rows.Next() {
		var column Column
		if err := rows.Scan(&column.Name, &column.Type, &column.Generated); err != nil {
			return nil, err
		}
		columns = append(columns, column)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(columns) == 0 {
		return nil, fmt.Errorf("table %s.%s does not exist or has no columns", schema, table)
	}
	return columns, nil
}
