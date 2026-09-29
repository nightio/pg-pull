package database

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/nightio/pg-pull/internal/dump"
	"github.com/nightio/pg-pull/internal/testpostgres"
	"github.com/jackc/pgx/v5"
)

type restoreFixture struct {
	dir       string
	manifest  dump.Manifest
	sequences []dump.Sequence
	schema    dump.Schema
	target    *Target
	admin     *pgx.Conn
}

func newRestoreFixture(t *testing.T, sourceDSN, targetDSN string, sourceSQL, targetSQL []string) restoreFixture {
	t.Helper()
	ctx := context.Background()
	sourceAdmin := connectTestDB(t, ctx, sourceDSN)
	defer sourceAdmin.Close(ctx)
	targetAdmin := connectTestDB(t, ctx, targetDSN)
	t.Cleanup(func() { targetAdmin.Close(ctx) })
	execStatements(t, ctx, sourceAdmin, `DROP SCHEMA IF EXISTS source_data CASCADE`, `CREATE SCHEMA source_data`)
	execStatements(t, ctx, targetAdmin, `DROP SCHEMA IF EXISTS target_data CASCADE`, `CREATE SCHEMA target_data`)
	execStatements(t, ctx, sourceAdmin, sourceSQL...)
	execStatements(t, ctx, targetAdmin, targetSQL...)
	config := configFromDSN(t, sourceDSN, "source_data")
	source, err := OpenSource(ctx, config, config.Password, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close(ctx)
	snapshot, err := source.BeginSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	tables, err := snapshot.TablesForPatterns(ctx, "source_data", []string{".+"})
	snapshot.Rollback(ctx)
	if err != nil {
		t.Fatal(err)
	}
	dir := exportFixture(t, ctx, source, tables)
	manifest, sequences, err := dump.Load(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	schema, err := dump.LoadSchema(dir)
	if err != nil {
		t.Fatal(err)
	}
	target, err := OpenTarget(ctx, configFromDSN(t, targetDSN, "target_data"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { target.Close(ctx) })
	return restoreFixture{dir, manifest, sequences, schema, target, targetAdmin}
}

func (f restoreFixture) restore(ctx context.Context, selected, create map[string]struct{}) error {
	return f.target.Restore(ctx, f.dir, f.manifest, f.sequences, selected, f.schema, create, nil)
}

func assertSequenceState(t *testing.T, conn *pgx.Conn, name string, value int64, called bool) {
	t.Helper()
	var got int64
	var isCalled bool
	if err := conn.QueryRow(context.Background(), "SELECT last_value, is_called FROM "+quoteIdentifier("target_data", name)).Scan(&got, &isCalled); err != nil {
		t.Fatal(err)
	}
	if got != value || isCalled != called {
		t.Fatalf("%s state = (%d, %t), want (%d, %t)", name, got, isCalled, value, called)
	}
}

func TestRestoreSchemaRegressions(t *testing.T) {
	if os.Getenv("PG_PULL_INTEGRATION") == "" {
		t.Skip("set PG_PULL_INTEGRATION=1 to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	sourceDSN := testpostgres.DSN(t, "PG_PULL_TEST_SOURCE_DSN", "postgres:15-alpine")
	targetDSN := testpostgres.DSN(t, "PG_PULL_TEST_TARGET_DSN", "postgres:18-alpine")
	for _, standalone := range []bool{false, true} {
		name := "foreign_key_to_primary_key"
		parent := `CREATE TABLE source_data.items (id integer PRIMARY KEY)`
		extra := `CREATE INDEX items_extra ON source_data.items(id)`
		if standalone {
			name = "foreign_key_to_standalone_unique_index"
			parent = `CREATE TABLE source_data.items (id integer)`
			extra = `CREATE UNIQUE INDEX items_key ON source_data.items(id)`
		}
		t.Run(name, func(t *testing.T) {
			f := newRestoreFixture(t, sourceDSN, targetDSN, []string{parent, extra,
				`CREATE TABLE source_data.children (id integer PRIMARY KEY, item integer REFERENCES source_data.items(id))`,
				`INSERT INTO source_data.items VALUES (1)`, `INSERT INTO source_data.children VALUES (2,1)`,
			}, nil)
			if f.manifest.Tables[0].Name != "children" {
				t.Fatal("fixture must use application alphabetical ordering")
			}
			parentOnly := map[string]struct{}{"items": {}}
			if err := f.restore(ctx, parentOnly, parentOnly); err != nil {
				t.Fatal(err)
			}
			if _, err := f.admin.Exec(ctx, `INSERT INTO target_data.items VALUES (1)`); err == nil {
				t.Fatal("parent lost uniqueness")
			}
			execStatements(t, ctx, f.admin, `DROP SCHEMA target_data CASCADE`, `CREATE SCHEMA target_data`)
			both := map[string]struct{}{"children": {}, "items": {}}
			if err := f.restore(ctx, both, both); err != nil {
				t.Fatal(err)
			}
			var count int
			if err := f.admin.QueryRow(ctx, `SELECT count(*) FROM target_data.children c JOIN target_data.items i ON i.id=c.item`).Scan(&count); err != nil || count != 1 {
				t.Fatalf("restored relation: count=%d err=%v", count, err)
			}
		})
	}
	t.Run("finalization_failure_preserves_unowned_sequence", func(t *testing.T) {
		f := newRestoreFixture(t, sourceDSN, targetDSN, []string{
			`CREATE SEQUENCE source_data.counter`, `CREATE TABLE source_data.a (id bigint DEFAULT nextval('source_data.counter'))`,
			`INSERT INTO source_data.a VALUES (11)`, `SELECT setval('source_data.counter',40,true)`,
			`CREATE TABLE source_data.z (id integer)`, `INSERT INTO source_data.z VALUES (1)`, `CREATE INDEX z_key ON source_data.z(id)`,
		}, []string{
			`CREATE SEQUENCE target_data.counter`, `CREATE TABLE target_data.a (id bigint DEFAULT nextval('target_data.counter'))`,
			`INSERT INTO target_data.a VALUES (77)`, `SELECT setval('target_data.counter',500,false)`,
			`CREATE TABLE target_data.other (id integer)`, `CREATE INDEX z_key ON target_data.other(id)`,
		})
		if err := f.restore(ctx, map[string]struct{}{"a": {}, "z": {}}, map[string]struct{}{"z": {}}); err == nil {
			t.Fatal("expected index-name collision")
		}
		assertSequenceState(t, f.admin, "counter", 500, false)
		var id int64
		if err := f.admin.QueryRow(ctx, `SELECT id FROM target_data.a`).Scan(&id); err != nil || id != 77 {
			t.Fatalf("original row: id=%d err=%v", id, err)
		}
		var absent bool
		if err := f.admin.QueryRow(ctx, `SELECT to_regclass('target_data.z') IS NULL`).Scan(&absent); err != nil || !absent {
			t.Fatalf("created table survived rollback: %v", err)
		}
	})
	t.Run("later_sequence_failure_rolls_back_previous_setval", func(t *testing.T) {
		f := newRestoreFixture(t, sourceDSN, targetDSN, []string{
			`CREATE SEQUENCE source_data.counter1`, `CREATE SEQUENCE source_data.counter2`,
			`CREATE TABLE source_data.a (id bigint DEFAULT nextval('source_data.counter1'), other bigint DEFAULT nextval('source_data.counter2'))`,
			`INSERT INTO source_data.a VALUES (11,12)`, `SELECT setval('source_data.counter1',40,true)`, `SELECT setval('source_data.counter2',150,false)`,
		}, []string{
			`CREATE SEQUENCE target_data.counter1`, `CREATE SEQUENCE target_data.counter2 MAXVALUE 100`,
			`CREATE TABLE target_data.a (id bigint DEFAULT nextval('target_data.counter1'), other bigint DEFAULT nextval('target_data.counter2'))`,
			`INSERT INTO target_data.a VALUES (77,78)`, `SELECT setval('target_data.counter1',500,false)`, `SELECT setval('target_data.counter2',50,true)`,
		})
		selected := map[string]struct{}{"a": {}}
		if err := f.restore(ctx, selected, nil); err == nil {
			t.Fatal("expected second sequence to exceed target bounds")
		}
		assertSequenceState(t, f.admin, "counter1", 500, false)
		assertSequenceState(t, f.admin, "counter2", 50, true)
		var id int64
		if err := f.admin.QueryRow(ctx, `SELECT id FROM target_data.a`).Scan(&id); err != nil || id != 77 {
			t.Fatalf("original row: id=%d err=%v", id, err)
		}
		execStatements(t, ctx, f.admin, `ALTER SEQUENCE target_data.counter2 NO MAXVALUE`)
		if err := f.restore(ctx, selected, nil); err != nil {
			t.Fatal(err)
		}
		assertSequenceState(t, f.admin, "counter1", 40, true)
		assertSequenceState(t, f.admin, "counter2", 150, false)
	})
	t.Run("shared_sequence_keeps_owner_when_consumer_is_skipped", func(t *testing.T) {
		f := newRestoreFixture(t, sourceDSN, targetDSN, []string{
			`CREATE TABLE source_data.a (id bigserial PRIMARY KEY)`,
			`CREATE TABLE source_data.b (id bigint DEFAULT nextval('source_data.a_id_seq'))`,
			`INSERT INTO source_data.a VALUES (41)`, `SELECT setval('source_data.a_id_seq',42,true)`,
		}, []string{`CREATE TABLE target_data.a (id bigserial PRIMARY KEY)`, `INSERT INTO target_data.a VALUES (7)`})
		seq := f.schema.Sequences[0]
		if seq.OwnerTable != "a" || seq.OwnerColumn != "id" || !sameStrings(seq.ReferencedTables, []string{"a", "b"}) {
			t.Fatalf("incorrect sequence ownership: %#v", seq)
		}
		if err := f.restore(ctx, map[string]struct{}{"a": {}}, nil); err != nil {
			t.Fatal(err)
		}
		assertSequenceState(t, f.admin, "a_id_seq", 42, true)
		execStatements(t, ctx, f.admin, `INSERT INTO target_data.a DEFAULT VALUES`)
		if err := f.restore(ctx, map[string]struct{}{"b": {}}, map[string]struct{}{"b": {}}); err != nil {
			t.Fatal(err)
		}
		assertSequenceState(t, f.admin, "a_id_seq", 43, true)
		var owner string
		if err := f.admin.QueryRow(ctx, `SELECT pg_get_serial_sequence('target_data.a','id')`).Scan(&owner); err != nil || owner != "target_data.a_id_seq" {
			t.Fatalf("owner changed: %s %v", owner, err)
		}
	})
	t.Run("unowned_shared_sequence_created_once", func(t *testing.T) {
		f := newRestoreFixture(t, sourceDSN, targetDSN, []string{
			`CREATE SEQUENCE source_data.shared`,
			`CREATE TABLE source_data.a (id bigint DEFAULT nextval('source_data.shared'))`,
			`CREATE TABLE source_data.b (id bigint DEFAULT nextval('source_data.shared'))`,
			`INSERT INTO source_data.a DEFAULT VALUES`, `INSERT INTO source_data.b DEFAULT VALUES`,
		}, nil)
		selected := map[string]struct{}{"a": {}, "b": {}}
		if err := f.restore(ctx, selected, selected); err != nil {
			t.Fatal(err)
		}
		assertSequenceState(t, f.admin, "shared", 2, true)
		var unowned bool
		if err := f.admin.QueryRow(ctx, `SELECT NOT EXISTS (SELECT 1 FROM pg_depend WHERE classid='pg_class'::regclass AND objid='target_data.shared'::regclass AND deptype IN ('a','i'))`).Scan(&unowned); err != nil || !unowned {
			t.Fatalf("unexpected sequence ownership: %v", err)
		}
	})
	t.Run("missing_sequence_with_skipped_owner_is_rejected", func(t *testing.T) {
		f := newRestoreFixture(t, sourceDSN, targetDSN, []string{
			`CREATE TABLE source_data.a (id bigserial)`,
			`CREATE TABLE source_data.b (id bigint DEFAULT nextval('source_data.a_id_seq'))`,
		}, nil)
		selected := map[string]struct{}{"b": {}}
		if err := f.restore(ctx, selected, selected); err == nil {
			t.Fatal("expected missing owner error")
		}
		var absent bool
		if err := f.admin.QueryRow(ctx, `SELECT to_regclass('target_data.a_id_seq') IS NULL AND to_regclass('target_data.b') IS NULL`).Scan(&absent); err != nil || !absent {
			t.Fatalf("partial creation survived: %v", err)
		}
	})
	for _, conflict := range []struct {
		name, setup, want string
	}{
		{"sequence_owned_by_other_table", `CREATE TABLE target_data.other (id bigint); CREATE SEQUENCE target_data.shared OWNED BY target_data.other.id`, "belongs to other.id"},
		{"sequence_name_is_table", `CREATE TABLE target_data.shared (id bigint)`, "is not a sequence"},
	} {
		t.Run(conflict.name, func(t *testing.T) {
			f := newRestoreFixture(t, sourceDSN, targetDSN, []string{
				`CREATE SEQUENCE source_data.shared`,
				`CREATE TABLE source_data.a (id bigint DEFAULT nextval('source_data.shared'))`,
			}, []string{conflict.setup})
			selected := map[string]struct{}{"a": {}}
			if err := f.restore(ctx, selected, selected); err == nil || !strings.Contains(err.Error(), conflict.want) {
				t.Fatalf("expected %q, got %v", conflict.want, err)
			}
			var absent bool
			if err := f.admin.QueryRow(ctx, `SELECT to_regclass('target_data.a') IS NULL`).Scan(&absent); err != nil || !absent {
				t.Fatalf("partial creation survived: %v", err)
			}
		})
	}
	t.Run("column_collation_is_preserved", func(t *testing.T) {
		f := newRestoreFixture(t, sourceDSN, targetDSN, []string{`CREATE TABLE source_data.items (name text COLLATE "C")`, `INSERT INTO source_data.items VALUES ('a')`}, nil)
		selected := map[string]struct{}{"items": {}}
		if err := f.restore(ctx, selected, selected); err != nil {
			t.Fatal(err)
		}
		var name string
		if err := f.admin.QueryRow(ctx, `SELECT co.collname FROM pg_attribute a JOIN pg_collation co ON co.oid=a.attcollation WHERE a.attrelid='target_data.items'::regclass AND a.attname='name'`).Scan(&name); err != nil || name != "C" {
			t.Fatalf("collation=%s err=%v", name, err)
		}
	})
	t.Run("missing_custom_collation_fails_before_truncation", func(t *testing.T) {
		f := newRestoreFixture(t, sourceDSN, targetDSN, []string{
			`CREATE COLLATION source_data.custom (provider=libc, locale='C')`,
			`CREATE TABLE source_data.a (id integer)`, `INSERT INTO source_data.a VALUES(1)`,
			`CREATE TABLE source_data.z (name text COLLATE source_data.custom)`, `INSERT INTO source_data.z VALUES('a')`,
		}, []string{`CREATE TABLE target_data.a (id integer)`, `INSERT INTO target_data.a VALUES(99)`})
		selected, create := map[string]struct{}{"a": {}, "z": {}}, map[string]struct{}{"z": {}}
		if err := f.restore(ctx, selected, create); err == nil {
			t.Fatal("expected unavailable collation error")
		}
		var id int
		if err := f.admin.QueryRow(ctx, `SELECT id FROM target_data.a`).Scan(&id); err != nil || id != 99 {
			t.Fatalf("original row changed: %d %v", id, err)
		}
		execStatements(t, ctx, f.admin, `CREATE COLLATION target_data.custom (provider=libc, locale='C')`)
		if err := f.restore(ctx, selected, create); err != nil {
			t.Fatal(err)
		}
	})
}
