package app

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/nightio/pg-pull/internal/cli"
	"github.com/nightio/pg-pull/internal/config"
	"github.com/nightio/pg-pull/internal/database"
	"github.com/nightio/pg-pull/internal/dump"
	"github.com/nightio/pg-pull/internal/testpostgres"
	"github.com/jackc/pgx/v5"
)

func TestParallelExportFailureDoesNotCompleteDump(t *testing.T) {
	if os.Getenv("PG_PULL_INTEGRATION") == "" {
		t.Skip("set PG_PULL_INTEGRATION=1 to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	dsn := testpostgres.DSN(t, "PG_PULL_TEST_SOURCE_DSN", "postgres:15-alpine")
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)
	for _, sql := range []string{
		`DROP SCHEMA IF EXISTS app_export_data CASCADE`,
		`CREATE SCHEMA app_export_data`,
		`CREATE TABLE app_export_data.first (id bigint)`,
		`CREATE TABLE app_export_data.second (id bigint)`,
		`INSERT INTO app_export_data.first VALUES (1)`,
		`INSERT INTO app_export_data.second VALUES (2)`,
	} {
		if _, err := admin.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	defer admin.Exec(context.Background(), `DROP SCHEMA IF EXISTS app_export_data CASCADE`) //nolint:errcheck
	parsed, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	sourceConfig := config.Database{
		Host: parsed.Host, Port: parsed.Port, DBName: parsed.Database,
		User: parsed.User, Schema: "app_export_data", SSLMode: "disable",
	}
	source, err := database.OpenSource(ctx, sourceConfig, parsed.Password, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close(ctx)
	input, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	a := &App{UI: cli.New(ctx, input, io.Discard, io.Discard)}
	prepared := []preparedTable{
		{entry: dump.Table{Name: "first", File: "tables/0001.copy.gz"}, columns: []string{"id"}},
		{entry: dump.Table{Name: "second", File: "tables/0002.copy.gz"}, columns: []string{"id"}},
	}
	snapshot, err := source.BeginSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	entries, err := a.exportTables(ctx, source, snapshot, dir, "app_export_data", prepared, nil)
	_ = snapshot.Rollback(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].Name != "first" || entries[1].Name != "second" || entries[0].RowCount != 1 || entries[1].RowCount != 1 {
		t.Fatalf("unexpected ordered export results: %#v", entries)
	}

	prepared[1].entry.Name = "missing"
	snapshot, err = source.BeginSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	failureDir := t.TempDir()
	entries, err = a.exportTables(ctx, source, snapshot, failureDir, "app_export_data", prepared, nil)
	_ = snapshot.Rollback(ctx)
	if err == nil || entries != nil {
		t.Fatalf("failed export returned entries=%#v, error=%v", entries, err)
	}
	if _, err := os.Stat(filepath.Join(failureDir, "manifest.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed export created a reusable manifest: %v", err)
	}
}

func TestExportReleasesSourceSnapshot(t *testing.T) {
	if os.Getenv("PG_PULL_INTEGRATION") == "" {
		t.Skip("set PG_PULL_INTEGRATION=1 to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	dsn := testpostgres.DSN(t, "PG_PULL_TEST_SOURCE_DSN", "postgres:15-alpine")
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)
	for _, sql := range []string{
		`DROP SCHEMA IF EXISTS app_snapshot_data CASCADE`, `CREATE SCHEMA app_snapshot_data`,
		`CREATE TABLE app_snapshot_data.first(id integer)`, `CREATE TABLE app_snapshot_data.second(id integer)`,
		`INSERT INTO app_snapshot_data.first VALUES(1)`, `INSERT INTO app_snapshot_data.second VALUES(2)`,
	} {
		if _, err := admin.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	parsed, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Database{Host: parsed.Host, Port: parsed.Port, DBName: parsed.Database, User: parsed.User, Schema: "app_snapshot_data", SSLMode: "disable"}
	input, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	a := &App{UI: cli.New(ctx, input, io.Discard, io.Discard)}
	for _, tc := range []struct {
		name     string
		parallel int
		fail     bool
	}{
		{"single", 1, false}, {"parallel", 2, false}, {"failure_after_copy", 2, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source, err := database.OpenSource(ctx, cfg, parsed.Password, tc.parallel)
			if err != nil {
				t.Fatal(err)
			}
			defer source.Close(ctx)
			snapshot, err := source.BeginSnapshot(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer snapshot.Rollback(ctx)
			var refs []database.SequenceRef
			if tc.fail {
				refs = []database.SequenceRef{{Name: "missing_sequence"}}
			}
			_, _, err = a.export(ctx, source, snapshot, t.TempDir(), "test", cfg.Schema, nil, []string{"first", "second"}, refs)
			if (err != nil) != tc.fail {
				t.Fatalf("export error=%v, expected failure=%t", err, tc.fail)
			}
			// These locks would fail while the COPY transaction or any worker remained
			// open. Verify before deferred connection and snapshot cleanup takes place.
			tx, err := admin.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			if _, err := tx.Exec(ctx, `LOCK TABLE app_snapshot_data.first, app_snapshot_data.second IN ACCESS EXCLUSIVE MODE NOWAIT`); err != nil {
				t.Fatalf("source locks remain after export: %v", err)
			}
			if err := snapshot.Commit(ctx); !errors.Is(err, pgx.ErrTxClosed) {
				t.Fatalf("coordinator still open: %v", err)
			}
		})
	}
}
