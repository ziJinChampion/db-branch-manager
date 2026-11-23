# Connect existing migration tools

These examples use the same `dbbranch create` and `dbbranch connection` flow with two migration tools. Use a separate scratch database and archive for each example. Each baseline includes the tool's own migration history at its first revision. Run `pg_dump -Fc` after preparing the scratch database, then pass the archive to `dbbranch create` as described in the [quick start](../../README.md#quick-start).

## Python with Alembic

Install Alembic and `psycopg[binary]` in a virtual environment. Run the first migration with the scratch URL and the remaining migration with the PR URL.

```bash
DATABASE_URL="$BASELINE_URL" alembic -c examples/migrations/alembic/alembic.ini upgrade 0001
DATABASE_URL="$PREVIEW_URL" alembic -c examples/migrations/alembic/alembic.ini upgrade head
```

The restored `alembic_version` table starts at `0001` and advances to `0002`. The `posts.body` column is created in the preview database.

## Go with golang-migrate

Install the golang-migrate CLI with PostgreSQL support. Use the first command for the scratch database and the second for the PR database.

```bash
migrate -path examples/migrations/golang-migrate -database "$BASELINE_URL" up 1
migrate -path examples/migrations/golang-migrate -database "$PREVIEW_URL" up
```

The restored `schema_migrations` table starts at version `1` and advances to `2`. The `posts.rating` column is created in the preview database.
