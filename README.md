# ZenMoney Export

[![CI](https://github.com/pryid/zenmoney-export/actions/workflows/ci.yml/badge.svg)](https://github.com/pryid/zenmoney-export/actions/workflows/ci.yml) [![MCP Server](https://img.shields.io/badge/MCP-read--only-7C3AED)](doc/mcp.md) ![GitHub License](https://img.shields.io/github/license/pryid/zenmoney-export) ![Go Version](https://img.shields.io/github/go-mod/go-version/pryid/zenmoney-export) ![Latest Release](https://img.shields.io/github/v/release/pryid/zenmoney-export)

ZenMoney Export is a tool designed to export and sync data from the personal finance management
service [ZenMoney](https://zenmoney.ru/) to your own database.

## Features

- 🚀 Fast and reliable synchronization. Support Full and Incremental sync modes.
- 📊 Supports PostgreSQL (with plans for other databases).
- 🛠️ Easy-to-configure options for various use cases.
- 📦 Rootless Podman deployment for the exporter, PostgreSQL, and MCP.

## Requirements

- Podman and `podman-compose` 1.6 or later for the container deployment. On macOS and Windows, start a Podman machine first.
- PostgreSQL 15 through 18. PostgreSQL 14 and earlier cannot run the bundled migrations because
  migration `000003` uses `UNIQUE NULLS NOT DISTINCT`, which was introduced in PostgreSQL 15.

## Quick Start

Obtain an API token from ZenMoney by visiting [Budgera](https://budgera.com) or [Zerro.app](https://zerro.app/token) and following the instructions to
generate your token.

1. Copy `podman/.env.example` to `podman/.env` and replace the PostgreSQL credentials and
   ZenMoney API token. Ensure the credentials in `DB_URL` match the PostgreSQL variables.
2. Start PostgreSQL, run migrations, and start the exporter:

```bash
cp -n podman/.env.example podman/.env
# Edit podman/.env before starting the stack.
./podman/up
```

The script uses rootless Podman and `podman-compose` with the project name `zenexport`. It waits for
PostgreSQL and checks that migrations succeed before starting the exporter. To use another project
or environment file, set `ZENEXPORT_PROJECT_NAME` or `ZENEXPORT_ENV_FILE`. The initial startup
builds the exporter from this checkout. After changing source code, rebuild with
`podman-compose -p zenexport --env-file podman/.env -f podman/compose.yml build app`, then rerun
`./podman/up`. To use a schema-compatible prebuilt image instead, set `ZENEXPORT_IMAGE` and
`ZENEXPORT_PULL_POLICY=always` in `podman/.env`.

PostgreSQL 18 is only accessible on the private Podman network. `sslmode=disable` is intended
for this local development stack; use a TLS-verified connection URL for an external or production
database. PostgreSQL 18 stores its data under `/var/lib/postgresql`, where the named Podman volume
is mounted. Do not reuse a PostgreSQL 17-or-earlier data volume directly: perform a major-version
upgrade or restore a backup into a fresh PostgreSQL 18 volume.

An existing Docker installation and its volumes are separate from Podman. Back up the existing
database and restore it into the new Podman PostgreSQL volume before using this stack with existing
data; moving the configuration does not migrate the database automatically. A previous `docker/.env`
can be copied to `podman/.env` and adjusted for the new stack.

Stop the stack while preserving its volume:

```bash
podman-compose -p zenexport --env-file podman/.env -f podman/compose.yml down
```

Add `-f podman/compose.mcp.yml` before `down` when the stack was started with MCP. Avoid `down -v`
unless you intend to delete its database volume.

## Configuration

### Environment Variables

Global variables:

- `ZEN_API_TOKEN`: Your ZenMoney API token.
- `DB_URL`: Connection string for your database. Example: `postgres://user:password@localhost:5432/dbname`.
- `DB_TYPE`: Database type. Default: `postgres`.
- `LOG_LEVEL`: Log level for the exporter. Default: `info`.
- `ZEN_MAX_RESPONSE_SIZE_MB`: Maximum successful ZenMoney API response size in MiB. Default: `256`; increase it if a very large full sync exceeds the limit.

`TOKEN` and `DB_CONFIG` remain supported as legacy aliases for `ZEN_API_TOKEN` and `DB_URL`.
When both names are set, the canonical name takes precedence.

Command-specific variables:

- Refer to the command-specific help by running:

```bash
go run main.go --help
```

### File Configuration

Default configuration file is `~/.zenexport.yaml`. You can specify a custom file using the `--config` flag.

```yaml
db_type: postgres
db_url: "postgres://postgres:postgres@localhost:5432/postgres"
log_level: debug
token: not-a-real-token
max_response_size_mb: 256
```

The original `db_config` YAML key is also accepted for backward compatibility.

### Command-Line Arguments

Parameters can be set using environment variables or directly via command-line arguments.
Command-line flags take precedence over environment variables, which take precedence over the configuration file.

```bash
go run main.go sync --token=your-token-here --db-url=postgres://user:password@localhost:5432/dbname
```

Use `--config`, `--db-type`, `--db-url`, `--token`, `--log-level`, and `--max-response-size-mb` as global options.
Only PostgreSQL is currently supported.

## Commands

Now app supports the following commands:

- `sync`: Synchronize data from ZenMoney to your database.

## ZenMoney MCP (read-only)

`zenmcp` lets MCP-compatible AI clients analyze the data previously synchronized by `zenexport`.
It is a separate read-only server: report queries run in read-only PostgreSQL transactions, the
server never modifies financial data, and it does not need a ZenMoney API token.

Available tools cover spending summaries, cash flow, budget progress, transaction search, sync
freshness, and finance chart rendering.

### MCP quick start

Create one environment file for PostgreSQL, exporter, and MCP. Replace the credentials and API
token, then generate a bearer token with `openssl rand -hex 32` and save it as
`ZENMCP_BEARER_TOKEN`:

```bash
cp -n podman/.env.example podman/.env
openssl rand -hex 32
```

Start the exporter and MCP together. Compose merges both files into one project and default
network; `zenmcp` reuses `DB_URL` from `podman/.env` and connects to the same PostgreSQL service:

```bash
./podman/up --mcp
```

Register its Streamable HTTP endpoint in Codex. The client variable must contain the same bearer
token as `ZENMCP_BEARER_TOKEN` in `podman/.env`:

```bash
export ZENMCP_CLIENT_BEARER_TOKEN='paste-the-same-token-here'
codex mcp add zenmoney \
  --url http://127.0.0.1:8080/mcp \
  --bearer-token-env-var ZENMCP_CLIENT_BEARER_TOKEN
```

The server is ready when `curl --fail http://127.0.0.1:8080/readyz` succeeds. Compose publishes the
MCP endpoint on loopback only. See [MCP setup and operations](doc/mcp.md) for MCP Inspector, remote
deployment, and all configuration options. See [MCP architecture](doc/mcp-architecture.md) for tool
contracts and financial semantics.

### Sync Command

The `sync` command is used to synchronize data from ZenMoney to your database. If the command runs for the first time, it will
perform a full sync (in daemon mode, a full sync followed by incremental sync every `interval` minutes). Otherwise, it will perform
an incremental sync. You can force a full sync using `--force`.

Flags:

- `--batch-size int`: Set the maximum number of records sent in one database batch (default 1000). The full API response and sync cursor are still committed atomically.
- `-d`, `--daemon`: Run the sync in daemon mode, continuously syncing at intervals.
- `--dry-run`: Fetch data and print per-entity counts without saving data or advancing the sync cursor.
- `--entities string`: Specify which entities to sync (default "all").
- `--force`: Force a full sync, ignoring any previous sync state.
- `-h`, `--help`: Show help information for the sync command.
- `--interval int`: Set the sync interval in minutes (default 30).
- `--write-mode string`: Select `batch` (default) or the hybrid `copy` mode. In `copy` mode transactions are streamed into a temporary PostgreSQL staging table with `COPY` and merged with one upsert; other entities continue to use chunked batches. Both modes commit the full response and sync cursor in one transaction.

Example for full sync entities `transactions` and `accounts` plus getting the latest data:

```bash
go run main.go sync --entities transactions,accounts --force
```

For a large full sync, use the hybrid PostgreSQL `COPY` path:

```bash
go run main.go sync --force --write-mode copy
```

Example for incremental sync with a 5-minute interval:

```bash
go run main.go sync --interval 5 --daemon
```

Example for running full sync with a dry run:

```bash
go run main.go sync --dry-run
```

## Contributing

We welcome contributions! Please follow these steps:

1. Fork the repository.
2. Create a feature branch.
3. Commit your changes.
4. Submit a pull request.

### Running Tests

To ensure the project remains robust, run tests using:

```bash
go test ./...
```

### Linting

Linting uses [golangci-lint](https://golangci-lint.run/) v2 and [.golangci.yml](.golangci.yml).
Run `golangci-lint run ./...` locally. CI checks changes against the push or pull-request base;
the coverage report is available as a GitHub Actions artifact.

## Releasing

[Release Please](https://github.com/googleapis/release-please) maintains a release pull request from conventional commits merged into `main`. Merging that pull request creates the version tag and GitHub release, uploads GoReleaser artifacts, and publishes the versioned multi-architecture container image with Podman.

Images are published to `ghcr.io/pryid/zenmoney-export` with the built-in `GITHUB_TOKEN` (no
Docker Hub credentials are required). GitHub Packages creates new packages private by default;
make the package public in its settings if anonymous pulls are required. For a private package,
run `podman login ghcr.io` using a GitHub token with package read access before pulling. Set
`ZENEXPORT_IMAGE` to `ghcr.io/pryid/zenmoney-export:latest` and `ZENEXPORT_PULL_POLICY=always` to
use the published image. Enable GitHub Actions and allow it to create pull requests in the repository
settings.

Configure a repository secret named `RELEASE_PLEASE_TOKEN` with a personal access token if Release Please pull requests must trigger CI workflows. Without it, the workflow falls back to `GITHUB_TOKEN` and can still maintain releases, but GitHub does not start new workflow runs for events created by that token.

## License

This project is licensed under the MIT License. See the [LICENSE](LICENSE) file for details.

## Acknowledgments

Thanks to the ZenMoney team for their API and documentation, and to all contributors who help make this tool better.

### PostgreSQL integration tests

The integration tests start temporary PostgreSQL containers directly with Podman and remove them
when each test ends. They require a working rootless Podman installation and network access to pull
PostgreSQL images. Run the whole suite with `go test ./...`, or select a supported PostgreSQL
version, for example:

```bash
POSTGRES_VERSION=16 go test ./internal/db/postgres -run '^TestPostgresCompatibility$' -count=1
```

If Go runs inside a Toolbox container, run the tests on the host or make the host's `podman` command
available to the test process. The tests do not require a Docker-compatible socket.
