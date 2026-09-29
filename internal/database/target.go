package database

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/nightio/pg-pull/internal/config"
	"github.com/nightio/pg-pull/internal/dump"
	"github.com/jackc/pgx/v5"
)

type Target struct {
	conn   *pgx.Conn
	schema string
}

func OpenTarget(ctx context.Context, db config.Database) (*Target, error) {
	cfg, err := parseConnection(db, db.Password, false)
	if err != nil {
		return nil, err
	}
	connectCtx, cancel := context.WithTimeout(ctx, cfg.ConnectTimeout)
	defer cancel()
	conn, err := pgx.ConnectConfig(connectCtx, cfg)
	if err != nil {
		return nil, connectionError(db, "target", err)
	}
	return &Target{conn: conn, schema: db.Schema}, nil
}

func (t *Target) Close(ctx context.Context) error { return t.conn.Close(ctx) }

func (t *Target) Description() string { return t.conn.Config().Database + "." + t.schema }

func (t *Target) TablesForPatterns(ctx context.Context, patterns []string) ([]string, error) {
	if len(patterns) == 0 {
		return nil, nil
	}
	parts := make([]string, 0, len(patterns))
	for _, pattern := range patterns {
		parts = append(parts, "("+pattern+")")
	}
	rows, err := t.conn.Query(ctx, `
SELECT tablename
FROM pg_catalog.pg_tables
WHERE schemaname = $1 AND tablename ~ $2
ORDER BY tablename`, t.schema, strings.Join(parts, "|"))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []string
	for rows.Next() {
		var table string
		if err := rows.Scan(&table); err != nil {
			return nil, err
		}
		result = append(result, table)
	}
	return result, rows.Err()
}

func (t *Target) LocalTables(ctx context.Context) (map[string]struct{}, error) {
	rows, err := t.conn.Query(ctx, `SELECT tablename FROM pg_catalog.pg_tables WHERE schemaname = $1`, t.schema)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make(map[string]struct{})
	for rows.Next() {
		var table string
		if err := rows.Scan(&table); err != nil {
			return nil, err
		}
		result[table] = struct{}{}
	}
	return result, rows.Err()
}

func (t *Target) Columns(ctx context.Context, table string) ([]Column, error) {
	return queryColumns(ctx, t.conn, t.schema, table)
}

type RestoreProgress func(table string, read, total int64)

func (t *Target) Restore(
	ctx context.Context,
	dir string,
	manifest dump.Manifest,
	sequences []dump.Sequence,
	tables map[string]struct{},
	schema dump.Schema,
	create map[string]struct{},
	progress RestoreProgress,
) error {
	for _, table := range schema.Tables {
		if !selectedTable(create, table.Name) {
			continue
		}
		for _, col := range table.Columns {
			if col.Collation == nil {
				continue
			}
			name := targetCollation(*col.Collation, manifest.SourceSchema, t.schema)
			var exists bool
			if err := t.conn.QueryRow(ctx, "SELECT to_regcollation($1) IS NOT NULL", name).Scan(&exists); err != nil {
				return err
			}
			if !exists {
				return fmt.Errorf("cannot create %s.%s: required collation %s is unavailable", table.Name, col.Name, name)
			}
		}
	}
	tx, err := t.conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background()) // no-op after commit
	if len(create) > 0 {
		if err := createMissing(ctx, tx, t.schema, manifest.SourceSchema, schema, create); err != nil {
			return err
		}
	}

	if _, err := tx.Exec(ctx, "SET LOCAL session_replication_role = 'replica'"); err != nil {
		return fmt.Errorf("disable local triggers: %w", err)
	}
	for _, statement := range []string{
		"SET LOCAL DateStyle TO 'ISO'",
		"SET LOCAL IntervalStyle TO 'postgres'",
		"SET LOCAL extra_float_digits TO 3",
	} {
		if _, err := tx.Exec(ctx, statement); err != nil {
			return fmt.Errorf("configure COPY restore session: %w", err)
		}
	}
	quoted := make([]string, 0, len(tables))
	for _, table := range manifest.Tables {
		if _, ok := tables[table.Name]; ok {
			quoted = append(quoted, quoteIdentifier(t.schema, table.Name))
		}
	}
	if len(quoted) == 0 {
		return fmt.Errorf("no tables selected for restore")
	}
	if _, err := tx.Exec(ctx, "TRUNCATE TABLE "+strings.Join(quoted, ", ")+" RESTART IDENTITY"); err != nil {
		return fmt.Errorf("truncate local tables: %w", err)
	}

	for _, table := range manifest.Tables {
		if _, ok := tables[table.Name]; !ok {
			continue
		}
		reader, err := dump.OpenTable(dir, table)
		if err != nil {
			return fmt.Errorf("open table %s: %w", table.Name, err)
		}
		columns := make([]string, 0, len(table.Columns))
		for _, column := range table.Columns {
			columns = append(columns, column.Name)
		}
		counter := &countingReader{reader: reader, callback: func(read int64) {
			if progress != nil {
				progress(table.Name, read, table.UncompressedBytes)
			}
		}}
		copySQL := fmt.Sprintf(
			"COPY %s (%s) FROM STDIN WITH (FORMAT text)",
			quoteIdentifier(t.schema, table.Name), quotedColumns(columns),
		)
		tag, copyErr := tx.Conn().PgConn().CopyFrom(ctx, counter, copySQL)
		closeErr := reader.Close()
		if copyErr != nil {
			return fmt.Errorf("restore table %s: %w", table.Name, copyErr)
		}
		if closeErr != nil {
			return fmt.Errorf("close table %s stream: %w", table.Name, closeErr)
		}
		if tag.RowsAffected() != table.RowCount {
			return fmt.Errorf("restore table %s copied %d rows, expected %d", table.Name, tag.RowsAffected(), table.RowCount)
		}
		if progress != nil && table.UncompressedBytes == 0 {
			progress(table.Name, 0, 0)
		}
	}

	if len(create) > 0 {
		if err := finishCreated(ctx, tx, t.schema, schema, create); err != nil {
			return err
		}
	}
	definitions := make(map[string]dump.SequenceDefinition, len(schema.Sequences))
	for _, definition := range schema.Sequences {
		definitions[definition.Name] = definition
	}
	for _, sequence := range sequences {
		definition, ok := definitions[sequence.Name]
		if !ok {
			return fmt.Errorf("sequence %s lacks a definition", sequence.Name)
		}
		if definition.OwnerTable != "" {
			if !selectedTable(tables, definition.OwnerTable) {
				continue
			}
		} else if !sequenceNeeded(definition, tables) {
			continue
		}
		qualified := quoteIdentifier(t.schema, sequence.Name)
		// setval alone survives rollback. RESTART first replaces the sequence's
		// storage transactionally and locks it until commit, so a later failure
		// also rolls back setval, including for pre-existing unowned sequences.
		if _, err := tx.Exec(ctx, fmt.Sprintf("ALTER SEQUENCE %s RESTART WITH %d", qualified, sequence.LastValue)); err != nil {
			return fmt.Errorf("restart sequence %s: %w", sequence.Name, err)
		}
		if _, err := tx.Exec(ctx, "SELECT pg_catalog.setval($1::regclass, $2, $3)", qualified, sequence.LastValue, sequence.IsCalled); err != nil {
			return fmt.Errorf("restore sequence %s: %w", sequence.Name, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit local restore: %w", err)
	}
	return nil
}

type countingReader struct {
	reader   io.Reader
	read     int64
	callback func(int64)
}

func (r *countingReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	r.read += int64(n)
	if n > 0 && r.callback != nil {
		r.callback(r.read)
	}
	return n, err
}

func VerifyTargetColumns(table dump.Table, target []Column) error {
	writable := make(map[string]Column, len(target))
	for _, column := range target {
		writable[column.Name] = column
	}
	for _, column := range table.Columns {
		targetColumn, ok := writable[column.Name]
		if !ok {
			return fmt.Errorf("target table %s is missing column %s", table.Name, column.Name)
		}
		if targetColumn.Generated {
			return fmt.Errorf("target column %s.%s is generated and cannot accept copied data", table.Name, column.Name)
		}
	}
	return nil
}

func selectedTable(tables map[string]struct{}, name string) bool {
	_, ok := tables[name]
	return ok
}

func sequenceNeeded(sequence dump.SequenceDefinition, tables map[string]struct{}) bool {
	if selectedTable(tables, sequence.OwnerTable) {
		return true
	}
	for _, name := range sequence.ReferencedTables {
		if selectedTable(tables, name) {
			return true
		}
	}
	return false
}

func targetCollation(collation dump.Collation, sourceSchema, targetSchema string) string {
	if collation.Schema == sourceSchema {
		collation.Schema = targetSchema
	}
	return quoteIdentifier(collation.Schema, collation.Name)
}
