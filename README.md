# DB Branch Manager

`dbbranch` creates disposable PostgreSQL databases for pull request previews. Run it from CI or a local shell to give each PR its own database and credentials on a shared preview cluster.

When two PRs deploy against the same database, one PR's migrations can change the schema under the other. Closing a PR can also leave its schema changes behind. With `dbbranch`, each preview starts from a known baseline, runs its own migrations, and drops its database when the PR is merged or closed. Your project keeps its existing migration tool, such as Prisma, Alembic, Flyway, or golang-migrate.

## How it works

1. Publish a PostgreSQL custom archive for a trusted base commit. Include the migration history and any synthetic test data.
2. Create a PR database from that archive. Export its connection URL and pass it to your existing migration and deployment commands.
3. After the preview stops, destroy its database and role. The GitHub Actions examples handle merge and close events and periodically reconcile missed cleanup.

The current implementation targets PostgreSQL on a dedicated, non-production preview cluster. If two PRs pass in isolation, their combined migrations can still conflict. Run a migration check against the latest candidate merge result before merging.

## Requirements

- Go 1.25 and the PostgreSQL client tools. Use a client version that supports your server and baseline archive. The integration suite runs against PostgreSQL 16.
- Docker and `jq` for the local walkthrough below.
- A dedicated, non-production PostgreSQL cluster. Use a direct connection to the management database. Session advisory locks require a direct connection or a session-mode pooler.
- A management role that owns the management database and can create databases, create and administer preview roles, and terminate their sessions. On PostgreSQL 16, the setup below uses `CREATEDB`, `CREATEROLE`, and `pg_signal_backend`.
- TLS with certificate and host verification for remote connections. The connection URL defaults to `sslmode=verify-full` and accepts `sslrootcert`. Use `sslmode=disable` for a loopback-only test instance.

## Quick start

Use a fresh local instance for this example. The SQL uses disposable test passwords.

```bash
docker run -d --name dbbranch-local -p 127.0.0.1:55432:5432 \
  -e POSTGRES_PASSWORD=test-postgres-password postgres:16-alpine
until docker exec dbbranch-local pg_isready -h 127.0.0.1 -U postgres; do sleep 1; done
docker exec -i dbbranch-local psql -X -U postgres -v ON_ERROR_STOP=1 < scripts/test-postgres.sql
go build -o ./dbbranch ./cmd/dbbranch
admin_url='postgres://dbbranch_manager:test-manager-password@127.0.0.1:55432/dbbranch_admin?sslmode=disable'
```

Prepare a custom-format archive from a disposable database brought up to the base revision. Include the migration history and any synthetic seed data. For a minimal example that uses the local instance above:

```bash
docker exec dbbranch-local psql -X -U postgres -v ON_ERROR_STOP=1 -c 'CREATE DATABASE example_base'
docker exec dbbranch-local psql -X -U postgres -v ON_ERROR_STOP=1 -c 'REVOKE CONNECT ON DATABASE example_base FROM PUBLIC'
docker exec dbbranch-local psql -X -U postgres -d example_base -v ON_ERROR_STOP=1 \
  -c 'CREATE TABLE posts (id bigint PRIMARY KEY); CREATE TABLE migration_history (version int PRIMARY KEY); INSERT INTO migration_history VALUES (1)'
docker exec dbbranch-local pg_dump -U postgres -Fc example_base > baseline.dump
```

```bash
id=$(DBBRANCH_ADMIN_URL="$admin_url" ./dbbranch create --repo-id 12345 --pr 42 --revision merge123 \
  --baseline base123 --archive baseline.dump --json | jq -er .id)
credential_dir=$(mktemp -d)
DBBRANCH_ADMIN_URL="$admin_url" ./dbbranch connection --repo-id 12345 --pr 42 --id "$id" --file "$credential_dir/url"
export DATABASE_URL="$(cat "$credential_dir/url")"
rm -f "$credential_dir/url"
rmdir "$credential_dir"

# Run your existing migration and pass DATABASE_URL to the preview app.
# Example: pnpm exec prisma migrate deploy
# See examples/migrations for working Alembic and golang-migrate examples.

DBBRANCH_ADMIN_URL="$admin_url" ./dbbranch status --repo-id 12345 --pr 42 --json
# Stop the application before dropping its database.
DBBRANCH_ADMIN_URL="$admin_url" ./dbbranch destroy --repo-id 12345 --pr 42
```

In CI, inject the management URL only into steps that call `dbbranch`. Give migration and deployment steps only the isolated preview URL. Treat the archive as trusted input and keep both URLs out of logs.

## Resource behavior

- `create` is idempotent for the same repository ID, environment, PR number, revision, baseline, and attempt token. The registry checks the archive digest for repeat requests. Set a new `--attempt` to obtain a clean generation after a migration or deployment failure.
- `connection` writes the URL to a new mode-0600 file and marks the generation `in_use` before handing out its credentials. In-use generations persist until explicit cleanup.
- `destroy` removes the registered generations of one PR, or a single generation selected with `--id`. Repeating the command is safe.
- `gc` collects expired generations whose credentials were never handed out and retries incomplete deletions. Use `dbbranch gc --repo-id 12345 --older-than 24h --dry-run` to preview candidates.
- A management schema in the management database stores the registry and preview passwords. Restrict access to that database, PostgreSQL logs, and its backups. Revoke `CONNECT` from `PUBLIC` on all databases on the preview cluster, including management and baseline databases. The manager checks cross-database connect privileges before provisioning and before exporting credentials.
- Every preview owns its restored objects and has database-local `CREATE` privileges. PostgreSQL extensions that require elevated privileges need separate platform setup. The cluster shares compute, storage and cluster-level roles, so use the shared-cluster workflow for trusted contributors.

## GitHub Actions and migration examples

- [GitHub Actions](examples/github-actions/) has workflows for baseline publication, preview deployment, and cleanup. Supply your repository's migration and deployment hooks to use them.
- [Migration examples](examples/migrations/) show how Alembic and golang-migrate resume from the migration history restored with the baseline.

## Verification

```bash
go test ./...
go vet ./...
go build ./cmd/dbbranch
```

Set `DBBRANCH_TEST_ADMIN_URL` to the disposable management database initialized with `scripts/test-postgres.sql` to exercise the PostgreSQL integration suite. The tests create and remove isolated databases and roles.

[MIT license](LICENSE)
