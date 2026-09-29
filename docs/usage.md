# pg-pull usage guide

[← Back to README](../README.md)

Configuration, restore behavior and troubleshooting. For installation and your
first dump, start with the [quick start](../README.md#quick-start).

- [Installation and updates](#installation-and-updates)
- [Configuration](#configuration)
- [Terminal controls](#terminal-controls)
- [Operation and reusable dumps](#operation-and-reusable-dumps)
- [Connection and lock troubleshooting](#connection-and-lock-troubleshooting)
- [Development](#development)

## Installation and updates

To check for a newer release or update the installed executable:

```bash
pg-pull self-update --check
pg-pull self-update
```

The updater checks public GitHub releases without signing in. It shows download
progress and verifies the release binary against its SHA-256 sidecar before
installing it. It needs write access to the installed executable's directory.
On Windows, installation finishes after the running program exits; rerun
`pg-pull --version` afterward.

On macOS, a browser-downloaded binary may need its quarantine attribute removed:

```bash
xattr -d com.apple.quarantine /usr/local/bin/pg-pull
```

On Windows, rename the downloaded executable to `pg-pull.exe` and put it in a
directory on `PATH`, or run it from PowerShell as `.\pg-pull.exe`.

## Configuration

Create the starter configuration in the current project:

```bash
pg-pull init
```

Edit `.pg-pull/config.yaml`, then run:

```bash
pg-pull
```

Use an explicit configuration when needed:

```bash
pg-pull --config=/path/to/config.yaml
```

`pg-pull` searches for `.pg-pull/config.yaml` in the current directory and each
parent directory. `init` also creates `.pg-pull/.gitignore`; running it again
adds that file if the config already exists and the ignore file is missing.

```yaml
# Optional: required only for restore
# target:
#   host: 127.0.0.1
#   port: 5432
#   dbname: app
#   user: app
#   password: app # preferably set in config-local.yaml
#   schema: public
#   connect_timeout: 10s
#   lock_timeout: 10s

sources:
  staging:
    host: staging-db.internal
    port: 5432
    dbname: app
    user: readonly
    schema: public
    sslmode: require
    connect_timeout: 10s
    lock_timeout: 10s

modules:
  ALL:
    description: "Every table"
    patterns: ['.+']
  Settlement:
    description: "Settlement tables"
    patterns: ['^settlement_', '^settle_instant_']

# dump_dir: dumps
# parallel_exports: 2
```

An optional `.pg-pull/config-local.yaml` overrides individual fields in
`config.yaml`. For example, after defining the other `target` fields in the
shared config, keep its password in the local file:

```yaml
target:
  password: local-secret
```

Nested maps merge, while lists such as module `patterns` replace the shared
list. New sources and modules from the local file appear after shared entries.
With `--config`, the local override is read from `config-local.yaml` beside
the specified config file. `init` does not create `config-local.yaml`.

- `target` is optional. Add it to enable local restore. Put its password in
  `config-local.yaml` if the shared `config.yaml` is versioned.
- `sources` are remote databases. Their passwords are prompted and kept only in
  process memory.
- `schema` is optional and defaults to `public`. Source and target schemas may
  differ.
- Module patterns are PostgreSQL POSIX regular expressions matched against
  source tables.
- `dump_dir` defaults to the directory containing the config (normally `.pg-pull`);
  relative values resolve from that directory.
  The generated `.gitignore` ignores everything inside `.pg-pull` except
  `config.yaml` and `.gitignore`. A `dump_dir` outside `.pg-pull` needs its own
  Git ignore rule.
- `parallel_exports` controls simultaneous source-table downloads (1–8, default 2).
  Each worker uses a separate read-only connection and the same PostgreSQL snapshot.
- `connect_timeout` and `lock_timeout` are configured per source or target and
  both default to `10s`. Use duration strings such as `500ms`, `30s`, or `1m`;
  values must be positive whole milliseconds, up to `2147483647ms` (about 24 days).
  They can also be overridden in `config-local.yaml`.
  `connect_timeout` covers each connection's DNS lookup, TLS/authentication and
  source read-only verification. `lock_timeout` limits each database lock wait,
  including source reads and target table/sequence changes; it does not limit
  the overall duration of COPY or restore. A lock timeout aborts the operation;
  target transaction changes are rolled back. No automatic retry is performed.

## Terminal controls

Interactive prompts use a step-by-step terminal interface. Use ↑/↓ (or `j`/`k`)
and Enter to choose an operation, source, or existing dump. For modules, use Space to
select or deselect entries and Enter to continue. Source passwords are hidden
while typing. The confirmation before replacing local data defaults to **No**;
Ctrl-C cancels a prompt.

Export and restore progress shows the current table, bytes copied, elapsed time,
and a percentage when a size estimate is available. Fixed-width columns keep
progress bars aligned; long names are shortened and narrow terminals use a
compact layout. Completed tables leave a short result in the terminal history.
The final summary shows the table count, total elapsed time and COPY data size;
export also reports the compressed gzip size.

The dump picker shows a local date and time including seconds (for example,
`26 Sep 2026, 14:23:16`), source, table count and modules. Before restore, a preview
shows the target, source and data size, with each table on its own line under
**Replace**, **Create** or **Skip**.

Set `ACCESSIBLE=1` for screen-reader-friendly prompts. When output is redirected
or the terminal does not support interactive forms, the CLI uses its plain text
prompts and progress messages.

## Operation and reusable dumps

A run offers **Create dump** by default. When a target is configured, it also
offers **Create dump and restore**, plus **Restore existing dump** when a reusable
dump exists. Dump-only mode never connects to the target.

A fresh dump performs these steps:

1. Select a source and enter its password.
2. Connect with `default_transaction_read_only=on` and verify the server reports
   `transaction_read_only=on`.
3. Select modules; table patterns resolve in the source snapshot.
4. Capture the source table structure and export all selected tables from the
   same repeatable-read, read-only snapshot.
5. Save each table as a gzip-compressed PostgreSQL text COPY stream and validate
   the completed dump.

The source snapshot closes as soon as all COPY workers and sequence reads finish,
before local dump validation, the restore confirmation and target writes. Waiting
at the restore prompt therefore does not keep the export's source locks open.

For **Create dump and restore**, the target connection is checked before export.
If local tables are missing, choose **Skip** (default), **Abort**, or **Create**.
Create reconstructs ordinary tables, columns, supported constraints and indexes
(including standalone UNIQUE indexes), enums, domains, and sequences. Enum and
domain array columns and their required type dependencies are supported.

Named column collations must already exist on the target; source-local collation
names resolve in the configured target schema. Missing collations are detected
before the restore transaction begins. Advanced structures such as partitioned
or inherited tables, user triggers, rules, row-level security and unsupported
custom types are rejected for creation. These restrictions apply to the missing
tables selected for creation; an unsupported table elsewhere in the dump does
not prevent creating a supported table.

A final confirmation, defaulting to **No**, controls whether any local data is replaced.
Declining restore still leaves a complete reusable dump. Restore-only mode uses
the same missing-table choice and final confirmation without contacting the source.

Each dump is retained for later replay without contacting the source:

```text
.pg-pull/2026-09-18_14-30-00/
├── manifest.json
├── schema.json
├── sequences.json
└── tables/
    ├── 0001.copy.gz
    └── 0002.copy.gz
```

Dumps use the `pg-pull-dump/v1` format. The manifest records columns, row counts,
sizes, and SHA-256 checksums for table data and metadata. `schema.json` contains
the table, type and sequence definitions; `sequences.json` stores sequence
counters. All files and metadata are verified before the restore transaction begins.
Only the current `pg-pull-dump/v1` format is supported; legacy SQL dumps cannot
be replayed. A dump becomes reusable only after `manifest.json` is written last.
Incomplete dumps are excluded from the picker; partial data files are retained.

The local table creation, `TRUNCATE`, COPY operations, constraints, indexes, and
sequence updates run in one transaction. `TRUNCATE` does not use `CASCADE`;
unselected tables are never implicitly wiped.
An error or Ctrl-C rolls the transaction back, preserving the previous local data.

Shared sequences retain their owner. An owned sequence's counter is restored
only when its owner table is selected; an unowned sequence's counter is restored
when a selected table uses it. If a required sequence is absent and its owner is
not being created, restore fails. A failed restore also rolls back sequence
counter changes.

## Connection and lock troubleshooting

Errors distinguish password authentication, PostgreSQL access rules, permissions,
TLS, DNS, refused connections and timeouts. A password failure after a TLS
fallback is reported as an authentication failure. Connection errors never
display passwords or the driver's connection URL.

- **Password authentication failed:** check the selected database user and password.
- **TLS connection failed:** check `sslmode`, server TLS support, certificate trust
  and the hostname. The CLI does not automatically change your SSL configuration.
- **Access denied / permission denied:** check PostgreSQL authentication rules or
  privileges for the operation. Restore also needs permission to change
  `session_replication_role`.
- **Connection timed out / refused / hostname unresolved:** check the host, port,
  server availability and VPN/DNS. Increase `connect_timeout` if needed.
- **Could not acquire a database lock:** finish competing transactions (for
  example, an open transaction in your database editor), then retry. Increase
  `lock_timeout` if a longer wait is appropriate. A failed restore preserves
  the previous data.

## Development

Requirements: Go 1.26+. Install the pinned Mage task runner and download Go
dependencies once:

```bash
go install github.com/magefile/mage@v1.17.2
go mod download
```

Ensure your Go binary directory (usually `$(go env GOPATH)/bin`) is on `PATH`.
Run the CLI directly while developing, or build a native executable:

```bash
go run ./cmd/pg-pull init
go run ./cmd/pg-pull
go run ./cmd/pg-pull --config=/path/to/config.yaml
mage build
./pg-pull --version
```

On Windows, the native output is `pg-pull.exe`. Mage provides the same commands
on all supported development platforms:

```bash
mage vet
mage test
mage integration
mage dist
mage verify
```

`mage dist` cross-compiles and verifies all six binaries. `mage verify` can also
check an existing `dist` directory. Build metadata can be supplied through
`VERSION`, `COMMIT`, and `BUILD_DATE`.

The optional [compose.yml](../compose.yml) starts a local PostgreSQL 18 target for
development:

```bash
docker compose up -d target
```

It exposes port `5432` with database/user `app` and password `secret`; configure
these values under `target` if using this development database.

`mage integration` starts temporary PostgreSQL 15 and 18 instances using
Testcontainers, then cleans them up. This task needs a running Docker-API-compatible
container runtime. Native builds use Go and Mage. To use existing
databases instead, set `PG_PULL_TEST_SOURCE_DSN` and `PG_PULL_TEST_TARGET_DSN`.
Use dedicated disposable test databases: test fixtures create/drop schemas and
replace data. Do not point these variables at databases whose data you need.

The integration tests cover COPY round trips, read-only source connections,
parallel snapshot consistency and release, schema/type reconstruction, foreign
key and index ordering, collations, shared sequence ownership, sequence rollback,
connection handshake timeouts and lock timeouts on both source and target.
Unit tests also cover dump corruption/path validation, metadata consistency,
configuration defaults/overrides and actionable error messages.

The Makefile provides `make build`, `make test`, `make vet`, `make test-integration`,
and `make dist` as shortcuts to Mage. The [test workflow](../.github/workflows/test.yaml)
runs vet, unit tests and integration tests for pull requests and pushes to `main`.
Publishing a GitHub release runs the
[release workflow](../.github/workflows/release.yaml), which runs the same checks,
builds and verifies all six executables, then attaches them with SHA-256 sidecars.
CI runners need a Docker-API-compatible runtime for integration tests.
