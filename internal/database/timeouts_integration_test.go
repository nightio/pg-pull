package database

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nightio/pg-pull/internal/config"
	"github.com/nightio/pg-pull/internal/testpostgres"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestConnectionHandshakeTimeout(t *testing.T) {
	if os.Getenv("PG_PULL_INTEGRATION") == "" {
		t.Skip("set PG_PULL_INTEGRATION=1")
	}
	for _, source := range []bool{false, true} {
		name := "target"
		if source {
			name = "source"
		}
		t.Run(name, func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			done, stopped := make(chan struct{}), make(chan struct{})
			defer func() { close(done); listener.Close(); <-stopped }()
			go func() {
				defer close(stopped)
				conn, err := listener.Accept()
				if err != nil {
					return
				}
				defer conn.Close()
				<-done // accept TCP but never answer the PostgreSQL startup packet
			}()
			_, port, _ := net.SplitHostPort(listener.Addr().String())
			n, _ := strconv.Atoi(port)
			db := config.Database{Host: "127.0.0.1", Port: uint16(n), DBName: "app", User: "reader", SSLMode: "disable", ConnectTimeout: "150ms"}
			ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
			defer cancel()
			start := time.Now()
			if source {
				_, err = OpenSource(ctx, db, "secret", 1)
			} else {
				_, err = OpenTarget(ctx, db)
			}
			if err == nil || !strings.Contains(UserMessage(err), "Connection timed out") || time.Since(start) > 3*time.Second {
				t.Fatalf("handshake timeout failed: %v (%s)", err, time.Since(start))
			}
		})
	}
}

func TestDatabaseLockTimeouts(t *testing.T) {
	if os.Getenv("PG_PULL_INTEGRATION") == "" {
		t.Skip("set PG_PULL_INTEGRATION=1")
	}
	ctx := context.Background()
	sourceDSN := testpostgres.DSN(t, "PG_PULL_TEST_SOURCE_DSN", "postgres:15-alpine")
	targetDSN := testpostgres.DSN(t, "PG_PULL_TEST_TARGET_DSN", "postgres:18-alpine")
	t.Run("restore_rolls_back_on_lock_timeout", func(t *testing.T) {
		f := newRestoreFixture(t, sourceDSN, targetDSN, []string{
			`CREATE TABLE source_data.a (id bigserial)`, `INSERT INTO source_data.a VALUES (1)`,
			`CREATE TABLE source_data.z (id integer)`, `INSERT INTO source_data.z VALUES (2)`,
		}, []string{
			`CREATE TABLE target_data.a (id bigserial)`, `INSERT INTO target_data.a VALUES (99)`,
			`SELECT setval('target_data.a_id_seq', 100, false)`,
			`CREATE TABLE target_data.z (id integer)`, `INSERT INTO target_data.z VALUES (88)`,
		})
		cfg := configFromDSN(t, targetDSN, "target_data")
		cfg.LockTimeout = "150ms"
		target, err := OpenTarget(ctx, cfg)
		if err != nil {
			t.Fatal(err)
		}
		defer target.Close(ctx)
		f.target = target
		blocker, err := f.admin.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer blocker.Rollback(ctx)
		if _, err := blocker.Exec(ctx, `LOCK TABLE target_data.z IN ACCESS SHARE MODE`); err != nil {
			t.Fatal(err)
		}
		deadline, cancel := context.WithTimeout(ctx, 4*time.Second)
		defer cancel()
		err = f.restore(deadline, map[string]struct{}{"a": {}, "z": {}}, nil)
		assertLockTimeout(t, err)
		blocker.Rollback(ctx)
		var a, z int
		if err := f.admin.QueryRow(ctx, `SELECT a.id,z.id FROM target_data.a a CROSS JOIN target_data.z z`).Scan(&a, &z); err != nil || a != 99 || z != 88 {
			t.Fatalf("original rows changed: %d %d %v", a, z, err)
		}
		assertSequenceState(t, f.admin, "a_id_seq", 100, false)
		if err := f.restore(ctx, map[string]struct{}{"a": {}, "z": {}}, nil); err != nil {
			t.Fatalf("retry after releasing lock: %v", err)
		}
	})
	t.Run("source_workers_have_lock_timeout_and_remain_read_only", func(t *testing.T) {
		admin := connectTestDB(t, ctx, sourceDSN)
		defer admin.Close(ctx)
		execStatements(t, ctx, admin, `DROP SCHEMA IF EXISTS source_data CASCADE`, `CREATE SCHEMA source_data`, `CREATE TABLE source_data.items (id integer)`)
		cfg := configFromDSN(t, sourceDSN, "source_data")
		cfg.LockTimeout = "150ms"
		source, err := OpenSource(ctx, cfg, cfg.Password, 2)
		if err != nil {
			t.Fatal(err)
		}
		defer source.Close(ctx)
		blocker, err := admin.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer blocker.Rollback(ctx)
		if _, err := blocker.Exec(ctx, `LOCK TABLE source_data.items IN ACCESS EXCLUSIVE MODE`); err != nil {
			t.Fatal(err)
		}
		for _, conn := range append(source.workers, source.conn) {
			var readOnly string
			if err := conn.QueryRow(ctx, "SHOW transaction_read_only").Scan(&readOnly); err != nil || readOnly != "on" {
				t.Fatalf("read-only verification: %s %v", readOnly, err)
			}
			snapshot, err := beginSnapshot(ctx, conn, "")
			if err != nil {
				t.Fatal(err)
			}
			deadline, cancel := context.WithTimeout(ctx, 4*time.Second)
			_, err = snapshot.CopyTable(deadline, "source_data", "items", []string{"id"}, io.Discard)
			cancel()
			snapshot.Rollback(ctx)
			assertLockTimeout(t, err)
		}
	})
}

func assertLockTimeout(t *testing.T, err error) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "55P03" || !strings.Contains(UserMessage(err), "lock_timeout") {
		t.Fatalf("expected lock timeout, got %v", err)
	}
}
