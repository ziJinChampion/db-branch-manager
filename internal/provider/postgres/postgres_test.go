package postgres

import (
	"context"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"dbbranch/internal/lifecycle"
	"github.com/jackc/pgx/v5"
)

func TestConnectionInput(t *testing.T) {
	for _, raw := range []string{
		"postgres://manager@db/metadata?options=-c%20search_path%3Dpublic",
		"postgres://manager@db/metadata?user=other",
		"postgres://manager@db1,db2/metadata",
		"postgres://manager@db/",
	} {
		if _, err := parseEndpoint(raw); err == nil {
			t.Errorf("accepted unsupported URL %q", raw)
		}
	}
}

func TestPostgresLifecycle(t *testing.T) {
	dsn := os.Getenv("DBBRANCH_TEST_ADMIN_URL")
	if dsn == "" {
		t.Skip("set DBBRANCH_TEST_ADMIN_URL for PostgreSQL integration")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	admin, err := pgx.Connect(ctx, dsn)
	must(t, err)
	defer admin.Close(ctx)
	_, err = admin.Exec(ctx, `CREATE SCHEMA IF NOT EXISTS dbbranch_fixture;
		CREATE TABLE IF NOT EXISTS dbbranch_fixture.demo (id int PRIMARY KEY);
		CREATE TABLE IF NOT EXISTS dbbranch_fixture.migrations (version int PRIMARY KEY);
		INSERT INTO dbbranch_fixture.migrations VALUES (1) ON CONFLICT DO NOTHING`)
	must(t, err)
	archive := filepath.Join(t.TempDir(), "baseline.dump")
	dump := exec.CommandContext(ctx, "pg_dump", "--format=custom", "--schema=dbbranch_fixture", "--file", archive, "--dbname", dsn)
	if output, err := dump.CombinedOutput(); err != nil {
		t.Fatalf("pg_dump failed (%v): %s", err, output)
	}

	store, err := Open(ctx, dsn)
	must(t, err)
	defer store.Close()
	a := lifecycle.Scope{RepoID: time.Now().UnixNano(), Env: "test", PR: 41}
	b := a
	b.PR++
	defer store.Destroy(ctx, a, "")
	defer store.Destroy(ctx, b, "")

	otherStore, err := Open(ctx, dsn)
	must(t, err)
	defer otherStore.Close()
	type result struct {
		generation lifecycle.Generation
		err        error
	}
	created := make(chan result, 1)
	go func() {
		g, err := otherStore.Create(ctx, a, "commit1", "base1", "one", archive)
		created <- result{g, err}
	}()
	first, err := store.Create(ctx, a, "commit1", "base1", "one", archive)
	repeated := <-created
	must(t, err)
	must(t, repeated.err)
	if repeated.generation.ID != first.ID {
		t.Fatal("concurrent create returned a different ID")
	}
	second, err := store.Create(ctx, b, "commit1", "base1", "one", archive)
	must(t, err)
	if first.ID == second.ID {
		t.Fatal("two PRs share a database")
	}

	for i, g := range []lifecycle.Generation{first, second} {
		dsn, err := store.Connection(ctx, g.Scope, g.ID)
		must(t, err)
		conn, err := pgx.Connect(ctx, dsn)
		must(t, err)
		var version int
		must(t, conn.QueryRow(ctx, "SELECT version FROM dbbranch_fixture.migrations").Scan(&version))
		if version != 1 {
			t.Fatalf("unexpected baseline version %d", version)
		}
		ddl := "ALTER TABLE dbbranch_fixture.demo ADD COLUMN detail integer"
		otherDB := second.Database
		if i == 1 {
			ddl = "ALTER TABLE dbbranch_fixture.demo ADD COLUMN detail text"
			otherDB = first.Database
		}
		_, err = conn.Exec(ctx, ddl)
		must(t, err)
		must(t, conn.Close(ctx))
		other, err := url.Parse(dsn)
		must(t, err)
		other.Path = "/" + otherDB
		if foreignConn, err := pgx.Connect(ctx, other.String()); err == nil {
			foreignConn.Close(ctx)
			t.Fatal("PR role can connect to another PR's database")
		}
	}

	firstDB := pgx.Identifier{first.Database}.Sanitize()
	_, err = admin.Exec(ctx, "ALTER DATABASE "+firstDB+" ALLOW_CONNECTIONS false")
	must(t, err)
	if _, err := store.Connection(ctx, a, first.ID); err == nil {
		t.Fatal("exported credentials for a sealed database")
	}
	if _, err := store.Create(ctx, a, "commit1", "base1", "one", archive); err == nil {
		t.Fatal("reused a sealed database")
	}
	_, err = admin.Exec(ctx, "ALTER DATABASE "+firstDB+" ALLOW_CONNECTIONS true")
	must(t, err)

	badArchive := filepath.Join(t.TempDir(), "invalid.dump")
	must(t, os.WriteFile(badArchive, []byte("not a dump"), 0600))
	if _, err := store.Create(ctx, a, "commit2", "base1", "broken", badArchive); err == nil {
		t.Fatal("invalid restore succeeded")
	}
	abandoned, err := store.Create(ctx, a, "commit2", "base1", "retry", archive)
	must(t, err)
	found, err := store.GC(ctx, a.RepoID, a.Env, time.Now().Add(time.Minute), true)
	must(t, err)
	if !hasID(found, abandoned.ID) || hasID(found, first.ID) || hasID(found, second.ID) {
		t.Fatalf("unexpected collection candidates: %+v", found)
	}
	_, err = store.GC(ctx, a.RepoID, a.Env, time.Now().Add(time.Minute), false)
	must(t, err)

	// Simulate a crash between executing the nontransactional CREATE DATABASE
	// and recording its OID. The reserved database is still sealed.
	orphanID, err := lifecycle.NewID()
	must(t, err)
	orphan := lifecycle.DatabaseName(orphanID)
	_, err = admin.Exec(ctx, `INSERT INTO dbbranch.generations
		(repo_id, environment, pr, id, revision, baseline, attempt, state, digest, password)
		VALUES ($1,$2,$3,$4,'crashed','base1','one','provisioning','digest','unused')`,
		a.RepoID, a.Env, a.PR, orphanID)
	must(t, err)
	_, err = admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{orphan}.Sanitize()+" TEMPLATE template0 ALLOW_CONNECTIONS false")
	must(t, err)
	collected, err := store.GC(ctx, a.RepoID, a.Env, time.Now().Add(time.Minute), false)
	must(t, err)
	if !hasID(collected, orphanID) {
		t.Fatalf("sealed database without an OID was not collected: %+v", collected)
	}
	var exists bool
	must(t, admin.QueryRow(ctx, "SELECT EXISTS (SELECT FROM pg_database WHERE datname=$1)", orphan).Scan(&exists))
	if exists {
		t.Fatal("sealed database remained after GC")
	}

	must(t, store.Destroy(ctx, a, ""))
	must(t, store.Destroy(ctx, a, ""))
	requests, err := store.Status(ctx, a.RepoID, a.Env, 0)
	must(t, err)
	if len(requests) != 1 || requests[0].ID != second.ID {
		t.Fatalf("cleanup affected another PR: %+v", requests)
	}
}

func hasID(generations []lifecycle.Generation, id string) bool {
	for _, g := range generations {
		if g.ID == id {
			return true
		}
	}
	return false
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
