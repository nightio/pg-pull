package database

import (
	"context"
	"fmt"
	"io"
	"regexp"

	"github.com/nightio/pg-pull/internal/config"
	"github.com/jackc/pgx/v5"
)

type Source struct {
	conn    *pgx.Conn
	workers []*pgx.Conn
}

func OpenSource(ctx context.Context, db config.Database, password string, parallelism int) (*Source, error) {
	if parallelism < 1 || parallelism > config.MaxParallelExports {
		return nil, fmt.Errorf("source parallelism must be between 1 and %d", config.MaxParallelExports)
	}
	cfg, err := parseConnection(db, password, true)
	if err != nil {
		return nil, err
	}
	source := &Source{}
	for i := 0; i < parallelism; i++ {
		// Bound the entire handshake, including DNS, TLS/auth fallback attempts
		// and the read-only verification, not just individual socket dials.
		connectCtx, cancel := context.WithTimeout(ctx, cfg.ConnectTimeout)
		conn, err := pgx.ConnectConfig(connectCtx, cfg.Copy())
		if err != nil {
			cancel()
			_ = source.Close(context.Background())
			return nil, connectionError(db, "source", err)
		}
		var readOnly string
		if err := conn.QueryRow(connectCtx, "SHOW transaction_read_only").Scan(&readOnly); err != nil {
			cancel()
			_ = conn.Close(ctx)
			_ = source.Close(context.Background())
			return nil, fmt.Errorf("verify remote read-only state: %w", err)
		}
		cancel()
		if readOnly != "on" {
			_ = conn.Close(ctx)
			_ = source.Close(context.Background())
			return nil, fmt.Errorf("refusing to continue: remote transaction_read_only=%q, expected on", readOnly)
		}
		if source.conn == nil {
			source.conn = conn
		} else {
			source.workers = append(source.workers, conn)
		}
	}
	return source, nil
}

func (s *Source) Close(ctx context.Context) error {
	var first error
	for _, conn := range append([]*pgx.Conn{s.conn}, s.workers...) {
		if conn == nil {
			continue
		}
		if err := conn.Close(ctx); err != nil && first == nil {
			first = err
		}
	}
	return first
}

func (s *Source) Parallelism() int { return 1 + len(s.workers) }

func (s *Source) ServerVersion(ctx context.Context) (string, error) {
	var version string
	if err := s.conn.QueryRow(ctx, "SHOW server_version").Scan(&version); err != nil {
		return "", err
	}
	return version, nil
}

func (s *Source) Columns(ctx context.Context, schema, table string) ([]Column, error) {
	return queryColumns(ctx, s.conn, schema, table)
}

func (s *Source) HasSequence(ctx context.Context, schema, sequence string) (bool, error) {
	qualified := quoteIdentifier(schema, sequence)
	var exists bool
	if err := s.conn.QueryRow(ctx, "SELECT pg_catalog.to_regclass($1) IS NOT NULL", qualified).Scan(&exists); err != nil {
		return false, err
	}
	return exists, nil
}

type Snapshot struct {
	tx pgx.Tx
}

func (s *Source) BeginSnapshot(ctx context.Context) (*Snapshot, error) {
	return beginSnapshot(ctx, s.conn, "")
}

var snapshotIDPattern = regexp.MustCompile(`^[0-9A-Fa-f-]+$`)

func (s *Source) BeginImportedSnapshot(ctx context.Context, worker int, id string) (*Snapshot, error) {
	if worker < 0 || worker >= len(s.workers) {
		return nil, fmt.Errorf("source worker index %d is out of range", worker)
	}
	if !snapshotIDPattern.MatchString(id) {
		return nil, fmt.Errorf("invalid source snapshot identifier")
	}
	return beginSnapshot(ctx, s.workers[worker], id)
}

func beginSnapshot(ctx context.Context, conn *pgx.Conn, importID string) (*Snapshot, error) {
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{
		IsoLevel:   pgx.RepeatableRead,
		AccessMode: pgx.ReadOnly,
	})
	if err != nil {
		return nil, err
	}
	if importID != "" {
		// PostgreSQL requires a string literal here, not a bind parameter. The
		// server-issued identifier is validated before interpolation.
		if _, err := tx.Exec(ctx, "SET TRANSACTION SNAPSHOT '"+importID+"'"); err != nil {
			_ = tx.Rollback(ctx)
			return nil, fmt.Errorf("import source snapshot: %w", err)
		}
	}
	for _, statement := range []string{
		"SET LOCAL DateStyle TO 'ISO'",
		"SET LOCAL IntervalStyle TO 'postgres'",
		"SET LOCAL extra_float_digits TO 3",
	} {
		if _, err := tx.Exec(ctx, statement); err != nil {
			_ = tx.Rollback(ctx)
			return nil, err
		}
	}
	var readOnly string
	if err := tx.QueryRow(ctx, "SHOW transaction_read_only").Scan(&readOnly); err != nil || readOnly != "on" {
		_ = tx.Rollback(ctx)
		if err != nil {
			return nil, fmt.Errorf("verify source snapshot: %w", err)
		}
		return nil, fmt.Errorf("source snapshot is not read-only")
	}
	return &Snapshot{tx: tx}, nil
}

func (s *Snapshot) ExportID(ctx context.Context) (string, error) {
	var id string
	if err := s.tx.QueryRow(ctx, "SELECT pg_catalog.pg_export_snapshot()").Scan(&id); err != nil {
		return "", err
	}
	if !snapshotIDPattern.MatchString(id) {
		return "", fmt.Errorf("invalid exported snapshot identifier")
	}
	return id, nil
}

func (s *Snapshot) Commit(ctx context.Context) error   { return s.tx.Commit(ctx) }
func (s *Snapshot) Rollback(ctx context.Context) error { return s.tx.Rollback(ctx) }

func (s *Snapshot) Columns(ctx context.Context, schema, table string) ([]Column, error) {
	return queryColumns(ctx, s.tx, schema, table)
}

func (s *Snapshot) RelationSizes(ctx context.Context, schema string, tables []string) (map[string]int64, error) {
	rows, err := s.tx.Query(ctx, `
SELECT c.relname, pg_catalog.pg_relation_size(c.oid)
FROM pg_catalog.pg_class c
JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname = $1 AND c.relname = ANY($2::text[])`, schema, tables)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make(map[string]int64, len(tables))
	for rows.Next() {
		var name string
		var size int64
		if err := rows.Scan(&name, &size); err != nil {
			return nil, err
		}
		result[name] = size
	}
	return result, rows.Err()
}

func (s *Snapshot) CopyTable(ctx context.Context, schema, table string, columns []string, writer io.Writer) (int64, error) {
	if len(columns) == 0 {
		return 0, fmt.Errorf("table %s.%s has no writable columns", schema, table)
	}
	sql := fmt.Sprintf(
		"COPY (SELECT %s FROM %s) TO STDOUT WITH (FORMAT text)",
		quotedColumns(columns),
		quoteIdentifier(schema, table),
	)
	tag, err := s.tx.Conn().PgConn().CopyTo(ctx, writer, sql)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

func (s *Snapshot) SequenceState(ctx context.Context, schema, sequence string) (int64, bool, error) {
	sql := fmt.Sprintf("SELECT last_value, is_called FROM %s", quoteIdentifier(schema, sequence))
	var lastValue int64
	var isCalled bool
	if err := s.tx.QueryRow(ctx, sql).Scan(&lastValue, &isCalled); err != nil {
		return 0, false, err
	}
	return lastValue, isCalled, nil
}
