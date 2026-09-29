# AGENTS.md

## Project overview

`pg-pull` is a Go CLI that creates reusable dumps of selected tables from a remote
PostgreSQL database and optionally restores them into a local development database.
It connects directly to PostgreSQL through `pgx` and is distributed as standalone
executables for Linux, macOS, and Windows.

The default command is interactive. It offers dump-only, dump-and-restore when a
target is configured, and restore of an existing compatible dump.
`pg-pull init` creates a starter `.pg-pull/config.yaml` and a `.gitignore` that
excludes everything except those two files. It can add a missing `.gitignore`
without changing an existing config. Optional `config-local.yaml` merges over
the shared config.

## Commands

- `go install github.com/magefile/mage@v1.17.2` — install the pinned task runner.
- `go mod download` — download Go dependencies.
- `go run ./cmd/pg-pull` — run locally; append `init` to initialize configuration.
- `mage test` / `mage vet` — unit tests and static checks.
- `mage integration` — PostgreSQL COPY, safety, and rollback tests using Testcontainers.
- `mage build` — build the current-platform binary as `./pg-pull` (or `pg-pull.exe`).
- `mage dist` — build all six Linux, macOS, and Windows release binaries.
- `mage verify` — verify the six binaries in an existing `dist` directory (also run by `mage dist`).
- `make test`, `make vet`, `make test-integration`, `make build`, and `make dist`
  are shortcuts to Mage tasks.

Native builds use Go and Mage. Integration tests start temporary
PostgreSQL 15/18 containers, or use `PG_PULL_TEST_SOURCE_DSN` and
`PG_PULL_TEST_TARGET_DSN`. These must be dedicated disposable test databases:
fixtures create/drop schemas and replace data. `compose.yml` provides an
optional development PostgreSQL target.

GitHub PR and release workflows run vet, unit tests and integration tests. Releases
also build/verify all six executables and upload them with SHA-256 sidecars.

## Architecture

- `cmd/pg-pull` wires signal handling, version metadata, and the application.
- `internal/app` owns the fresh-dump and reuse workflows.
- `internal/config` discovers, parses, validates, and initializes configuration.
- `internal/database` separates read-only source access from target writes.
- `internal/dump` owns the `pg-pull-dump/v1` format, gzip streams, checksums, and validation.
- `internal/cli` owns prompts and progress output.
- `internal/testpostgres` supplies disposable PostgreSQL instances or test DSNs.

## Configuration and errors

- `parallel_exports` is 1–8 (default 2). All workers use the same source snapshot
  and must retain the read-only connection protections.
- Each source and target accepts `connect_timeout` and `lock_timeout`, both
  defaulting to `10s`. Values are positive whole-millisecond durations up to
  `2147483647ms`. Local configuration overrides follow the normal map merge.
- Bound each connection's DNS/TLS/authentication and source read-only verification.
  Apply lock timeouts to all source workers and the target. Lock timeouts limit
  individual lock waits, not total COPY duration. Do not add automatic retries.
- Use `database.UserMessage` for user-facing database errors. Keep operation
  context and actionable guidance for authentication, TLS, network, permission
  and lock failures. Never display raw pgx connection-parse errors: they can
  contain a URL with the password. Preserve explicitly configured SSL behavior.

## Critical invariants

- The remote source is never written to. `OpenSource` always sets
  `default_transaction_read_only=on`, verifies `transaction_read_only=on`, and
  exposes no write method. Exports use a repeatable-read, read-only transaction,
  closed as soon as COPY workers and sequence reads finish.
- Only `Target.Restore` writes data. Its table/type creation, `TRUNCATE`, COPY
  FROM, constraint/index creation, and sequence updates share one transaction.
  `TRUNCATE` never uses `CASCADE` so skipped tables remain untouched. Finalize
  keys and indexes before foreign keys and sequence updates. Sequence updates
  use transactional `ALTER SEQUENCE ... RESTART` before `setval`; `setval` alone
  does not roll back for existing unowned sequences.
- Every destructive restore requires an interactive confirmation whose default is no.
- Store sequence ownership separately from referencing tables; never infer or
  replace ownership from defaults. Restore owned sequence state only when the
  owner is selected, and unowned state when a referencing table is selected.
- Resolve custom array element types and transitive domain dependencies before
  creating tables. Unsupported features apply only to tables that use them.
  Preserve standalone UNIQUE indexes referenced by foreign keys.
- Preserve named column collations, remapping source-local names to the target
  schema. Required collations must exist before the restore transaction starts;
  do not silently substitute defaults.
- Dump checksums, paths, schema, tables, and columns are validated before the
  target transaction begins. Validate sequence ownership/reference consistency,
  unique sequence states, collation names and constraint kinds as well.
- Support only the current `pg-pull-dump/v1` schema; do not introduce compatibility
  readers or migrations.
- A dump becomes reusable only when `manifest.json` is written last. Empty failed
  directories are removed; partial data is preserved but excluded from reuse.
- Source passwords are prompted at runtime, cleared after connection setup, and
  never written to logs, arguments, configuration, or dump metadata.

## Conventions

- Keep source and target types separate; do not add a general database API that
  makes source writes possible.
- Use explicit, quoted schema/table/column identifiers for generated COPY SQL.
- Keep table data streaming; never buffer a complete table in memory.
- Explain safety and PostgreSQL-specific reasons in comments where the constraint
  would otherwise be easy to weaken.
- Add integration coverage for changes to COPY, transactions, sequence handling,
  schemas, source connection setup, connection errors or timeouts. Cover lock
  timeout rollback and preserve read-only behavior on every source worker.
