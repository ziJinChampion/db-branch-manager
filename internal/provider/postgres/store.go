package postgres

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"dbbranch/internal/lifecycle"
	"github.com/jackc/pgx/v5"
)

type Store struct {
	conn     *pgx.Conn
	endpoint url.URL
}

type record struct {
	lifecycle.Generation
	digest   string
	password string
	dbOID    int64
	roleOID  int64
}

const columns = `repo_id, environment, pr, id, revision, baseline, attempt, state,
	updated_at, digest, password, db_oid, role_oid`

func parseEndpoint(raw string) (url.URL, error) {
	endpoint, err := url.Parse(raw)
	if err != nil || (endpoint.Scheme != "postgres" && endpoint.Scheme != "postgresql") ||
		endpoint.User == nil || endpoint.User.Username() == "" || endpoint.Hostname() == "" ||
		strings.Contains(endpoint.Hostname(), ",") || len(endpoint.Path) < 2 ||
		strings.Contains(endpoint.Path[1:], "/") || endpoint.Fragment != "" {
		return url.URL{}, errors.New("DBBRANCH_ADMIN_URL must be a direct PostgreSQL URL with a user, host and management database")
	}
	params, err := url.ParseQuery(endpoint.RawQuery)
	if err != nil {
		return url.URL{}, errors.New("invalid PostgreSQL URL parameters")
	}
	for name, values := range params {
		if (name != "sslmode" && name != "sslrootcert") || len(values) != 1 {
			return url.URL{}, errors.New("only sslmode and sslrootcert URL parameters are supported")
		}
	}
	switch params.Get("sslmode") {
	case "":
		params.Set("sslmode", "verify-full")
	case "verify-full", "disable":
	default:
		return url.URL{}, errors.New("sslmode must be verify-full or disable (for local testing)")
	}
	endpoint.RawQuery = params.Encode()
	return *endpoint, nil
}

func Open(ctx context.Context, rawURL string) (*Store, error) {
	endpoint, err := parseEndpoint(rawURL)
	if err != nil {
		return nil, err
	}
	config, err := pgx.ParseConfig(endpoint.String())
	if err != nil {
		return nil, errors.New("invalid PostgreSQL management connection settings")
	}
	config.RuntimeParams = map[string]string{"search_path": "pg_catalog", "application_name": "dbbranch"}
	conn, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		return nil, errors.New("cannot connect to the PostgreSQL management database")
	}
	store := &Store{conn: conn, endpoint: endpoint}
	if err := store.initialize(ctx); err != nil {
		store.Close()
		return nil, fmt.Errorf("initialize management registry: %w", err)
	}
	return store, nil
}

func (s *Store) Close() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s.conn.Close(ctx)
}

func (s *Store) initialize(ctx context.Context) error {
	tx, err := s.conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(4516362065429106508)"); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `CREATE SCHEMA IF NOT EXISTS dbbranch;
		CREATE TABLE IF NOT EXISTS dbbranch.generations (
			repo_id bigint NOT NULL,
			environment text NOT NULL,
			pr bigint NOT NULL,
			id text PRIMARY KEY,
			revision text NOT NULL,
			baseline text NOT NULL,
			attempt text NOT NULL,
			digest text NOT NULL,
			state text NOT NULL CHECK (state IN ('provisioning','ready','in_use','deleting')),
			password text NOT NULL,
			db_oid bigint NOT NULL DEFAULT 0,
			role_oid bigint NOT NULL DEFAULT 0,
			updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
			UNIQUE (repo_id, environment, pr, revision, baseline, attempt)
		);
		REVOKE ALL ON SCHEMA dbbranch FROM PUBLIC;
		REVOKE ALL ON dbbranch.generations FROM PUBLIC`); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) lock(ctx context.Context, scope lifecycle.Scope) (func(), error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	if _, err := s.conn.Exec(ctx, "SELECT pg_advisory_lock($1)", scope.LockKey()); err != nil {
		s.Close()
		return nil, err
	}
	return func() {
		unlockCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := s.conn.Exec(unlockCtx, "SELECT pg_advisory_unlock($1)", scope.LockKey()); err != nil {
			s.Close()
		}
	}, nil
}

func readRecord(row pgx.Row) (record, error) {
	var r record
	err := row.Scan(&r.RepoID, &r.Env, &r.PR, &r.ID, &r.Revision, &r.Baseline, &r.Attempt,
		&r.State, &r.UpdatedAt, &r.digest, &r.password, &r.dbOID, &r.roleOID)
	r.Database = lifecycle.DatabaseName(r.ID)
	return r, err
}

func (s *Store) byID(ctx context.Context, scope lifecycle.Scope, id string) (record, error) {
	return readRecord(s.conn.QueryRow(ctx, "SELECT "+columns+`
		FROM dbbranch.generations WHERE repo_id=$1 AND environment=$2 AND pr=$3 AND id=$4`,
		scope.RepoID, scope.Env, scope.PR, id))
}

func (s *Store) list(ctx context.Context, repoID int64, env string, pr int64) ([]record, error) {
	if repoID <= 0 || !lifecycle.ValidToken(env) || pr < 0 {
		return nil, errors.New("positive repository ID and valid environment are required")
	}
	rows, err := s.conn.Query(ctx, "SELECT "+columns+`
		FROM dbbranch.generations WHERE repo_id=$1 AND environment=$2 AND ($3::bigint=0 OR pr=$3)
		ORDER BY pr, updated_at, id`, repoID, env, pr)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]record, 0)
	for rows.Next() {
		r, err := readRecord(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, r)
	}
	return result, rows.Err()
}

func (s *Store) Status(ctx context.Context, repoID int64, env string, pr int64) ([]lifecycle.Generation, error) {
	records, err := s.list(ctx, repoID, env, pr)
	if err != nil {
		return nil, err
	}
	result := make([]lifecycle.Generation, 0, len(records))
	for _, r := range records {
		result = append(result, r.Generation)
	}
	return result, nil
}

func (s *Store) state(ctx context.Context, id string, state lifecycle.State) error {
	_, err := s.conn.Exec(ctx, "UPDATE dbbranch.generations SET state=$2, updated_at=clock_timestamp() WHERE id=$1", id, state)
	return err
}
