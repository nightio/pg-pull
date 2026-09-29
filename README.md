# PG-pull

**Bring the PostgreSQL tables you need into your local dev database.**

Pick groups of tables, save a compressed dump, and restore it now or reuse it later.
An interactive CLI for Linux, macOS and Windows.

- **Read-only source.** PostgreSQL enforces it on every source connection.
- **Reusable dumps.** Export once; restore again without contacting the source.
- **One executable.** No Go installation or PostgreSQL client tools needed to run.

## Install

Download your binary from [GitHub Releases](https://github.com/nightio/pg-pull/releases):

| Platform | x86-64 / Intel | ARM64 / Apple Silicon |
| --- | --- | --- |
| Linux | `pg-pull-linux-x86_64` | `pg-pull-linux-aarch64` |
| macOS | `pg-pull-macos-x86_64` | `pg-pull-macos-aarch64` |
| Windows | `pg-pull-windows-x86_64.exe` | `pg-pull-windows-aarch64.exe` |

On macOS or Linux, make it executable and move it onto your `PATH`.
For example, on Apple Silicon:

```bash
chmod +x pg-pull-macos-aarch64
sudo mv pg-pull-macos-aarch64 /usr/local/bin/pg-pull
pg-pull --version
```

On Windows, rename it to `pg-pull.exe` and add its directory to `PATH`,
or run `.\pg-pull.exe` from PowerShell in that directory.

```bash
pg-pull self-update --check  # Check for a new release
pg-pull self-update          # Update (requires write access to the install directory)
```

[macOS quarantine and update details →](docs/usage.md#installation-and-updates)

## Quick start

**1. Initialize** in your project directory:

```bash
pg-pull init
```

**2. Configure** `.pg-pull/config.yaml` with your connection details and table groups:

```yaml
sources:
  staging:
    host: staging-db.internal
    port: 5432
    dbname: app
    user: readonly
    sslmode: require

modules:
  Orders:
    description: "Orders and their items"
    patterns: ['^orders$', '^order_items$']
```

Modules match table names using PostgreSQL regular expressions; `['.+']` selects
all tables. The default schema is `public`.

**3. Run**, choose **Create dump**, enter the source password and select modules:

```bash
pg-pull
```

Use ↑/↓ and Enter to navigate; Space toggles modules. Dumps are saved under
`.pg-pull/` by default. `init` creates ignore rules that keep dumps and local
settings out of Git while allowing the shared config to be committed.

## Restore locally

Add your local database to `.pg-pull/config-local.yaml` (ignored by Git):

```yaml
target:
  host: 127.0.0.1
  port: 5432
  dbname: app
  user: app
  password: local-secret
```

Run `pg-pull` again and choose:

| Mode | What happens |
| --- | --- |
| **Create dump** | Export selected tables; leave the target alone. |
| **Create dump and restore** | Export, then replace selected local tables' data. |
| **Restore existing dump** | Replay a saved dump without contacting the source. |

Restore options require a target; **Restore existing dump** also requires a saved
dump. Missing tables can be skipped, created where supported, or cause an abort.
The target user needs restore privileges, including permission to set
`session_replication_role`. See [restore behavior and limitations](docs/usage.md#operation-and-reusable-dumps).

## Safety

- Source connections are verified read-only; exports share a consistent snapshot.
- Source passwords are prompted at runtime and never saved to config or dumps.
- Replacing local data requires confirmation, defaulting to **No**.
- Dumps are validated before restore. Target changes run in one transaction;
  failure rolls them back. `TRUNCATE` never uses `CASCADE`.

## Development

Requires **Go 1.26+**; put your Go bin directory on `PATH`.

```bash
go install github.com/magefile/mage@v1.17.2
go mod download
mage build        # Build ./pg-pull (pg-pull.exe on Windows)
mage test
mage vet
mage integration  # Requires Docker; uses disposable PostgreSQL containers
```

**More:** [Configuration](docs/usage.md#configuration) ·
[Troubleshooting](docs/usage.md#connection-and-lock-troubleshooting) ·
[Development guide](docs/usage.md#development)
