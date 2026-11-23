package postgres

import (
	"context"
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"

	"dbbranch/internal/lifecycle"
	"github.com/jackc/pgx/v5"
)

func ident(name string) string { return pgx.Identifier{name}.Sanitize() }

func randomPassword() string {
	var random [32]byte
	rand.Read(random[:])
	return base64.RawURLEncoding.EncodeToString(random[:])
}

func scramVerifier(password string) (string, error) {
	var salt [16]byte
	rand.Read(salt[:])
	const iterations = 4096
	key, err := pbkdf2.Key(sha256.New, password, salt[:], iterations, 32)
	if err != nil {
		return "", err
	}
	derive := func(label string) []byte {
		mac := hmac.New(sha256.New, key)
		mac.Write([]byte(label))
		return mac.Sum(nil)
	}
	stored := sha256.Sum256(derive("Client Key"))
	encode := base64.StdEncoding.EncodeToString
	return fmt.Sprintf("SCRAM-SHA-256$%d:%s$%s:%s", iterations, encode(salt[:]),
		encode(stored[:]), encode(derive("Server Key"))), nil
}

func (s *Store) createRole(ctx context.Context, r record) error {
	verifier, err := scramVerifier(r.password)
	if err != nil {
		return err
	}
	tx, err := s.conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	role := lifecycle.RoleName(r.ID)
	// Only the SCRAM verifier goes into role DDL, where it may be logged by PostgreSQL.
	_, err = tx.Exec(ctx, "CREATE ROLE "+ident(role)+
		" LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS PASSWORD '"+verifier+"'")
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE dbbranch.generations SET role_oid=
		(SELECT oid::bigint FROM pg_roles WHERE rolname=$2) WHERE id=$1`, r.ID, role)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) createDB(ctx context.Context, r record) error {
	db := ident(r.Database)
	if _, err := s.conn.Exec(ctx, "CREATE DATABASE "+db+" TEMPLATE template0 ALLOW_CONNECTIONS false"); err != nil {
		return err
	}
	if _, err := s.conn.Exec(ctx, `UPDATE dbbranch.generations SET db_oid=
		(SELECT oid::bigint FROM pg_database WHERE datname=$2) WHERE id=$1`, r.ID, r.Database); err != nil {
		return err
	}

	if _, err := s.conn.Exec(ctx, "REVOKE ALL ON DATABASE "+db+" FROM PUBLIC"); err != nil {
		return err
	}
	role := lifecycle.RoleName(r.ID)
	if _, err := s.conn.Exec(ctx, "GRANT CONNECT, CREATE, TEMPORARY ON DATABASE "+db+" TO "+ident(role)); err != nil {
		return err
	}
	if _, err := s.conn.Exec(ctx, "ALTER DATABASE "+db+" ALLOW_CONNECTIONS true"); err != nil {
		return err
	}

	endpoint := s.endpoint
	endpoint.Path = "/" + r.Database
	endpoint.RawPath = ""
	admin, err := pgx.Connect(ctx, endpoint.String())
	if err != nil {
		return errors.New("cannot set permissions on the new database")
	}
	defer admin.Close(ctx)
	_, err = admin.Exec(ctx, "GRANT USAGE, CREATE ON SCHEMA public TO "+ident(role))
	return err
}

func (s *Store) checkIsolation(ctx context.Context, r record) error {
	var accessible string
	err := s.conn.QueryRow(ctx, `SELECT datname FROM pg_database
		WHERE datallowconn AND datname <> $1 AND has_database_privilege($2::name, oid, 'CONNECT')
		ORDER BY datname LIMIT 1`, r.Database, lifecycle.RoleName(r.ID)).Scan(&accessible)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return fmt.Errorf("preview role can connect to database %q; remove its PUBLIC CONNECT grant or use a separate preview cluster", accessible)
}

func (s *Store) checkResources(ctx context.Context, r record) error {
	var valid bool
	err := s.conn.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM pg_database AS d CROSS JOIN pg_roles AS r
		WHERE d.datname=$1 AND r.rolname=$2 AND d.oid::bigint=$3 AND r.oid::bigint=$4
			AND d.datallowconn AND r.rolcanlogin
			AND d.datdba=(SELECT oid FROM pg_roles WHERE rolname=current_user)
			AND has_database_privilege(r.oid, d.oid, 'CONNECT')
	)`, r.Database, lifecycle.RoleName(r.ID), r.dbOID, r.roleOID).Scan(&valid)
	if err != nil {
		return err
	}
	if !valid {
		return fmt.Errorf("generation %s no longer matches an accessible managed database and role; inspect the cluster or create a new attempt", r.ID)
	}
	return nil
}

func (s *Store) clientURL(r record, withPassword bool) url.URL {
	endpoint := s.endpoint
	role := lifecycle.RoleName(r.ID)
	endpoint.User = url.User(role)
	if withPassword {
		endpoint.User = url.UserPassword(role, r.password)
	}
	endpoint.Path = "/" + r.Database
	endpoint.RawPath = ""
	return endpoint
}

func (s *Store) restore(ctx context.Context, r record, archive *os.File) error {
	endpoint := s.clientURL(r, false)
	restore := exec.CommandContext(ctx, "pg_restore", "--format=custom", "--no-owner", "--no-acl",
		"--no-tablespaces", "--single-transaction", "--exit-on-error", "--no-password", "--dbname", endpoint.String())
	restore.Stdin = archive
	restore.Stderr = os.Stderr
	restore.Env = []string{"PATH=" + os.Getenv("PATH"), "PGPASSWORD=" + r.password, "PGCONNECT_TIMEOUT=10"}
	if err := restore.Run(); err != nil {
		return fmt.Errorf("pg_restore failed: %w", err)
	}
	return nil
}

func (s *Store) removeResources(ctx context.Context, r record) error {
	if !lifecycle.ValidID(r.ID) {
		return errors.New("invalid managed database ID")
	}
	db := lifecycle.DatabaseName(r.ID)
	var dbOID int64
	var owned, acceptingConnections bool
	err := s.conn.QueryRow(ctx, `SELECT oid::bigint, datdba = (SELECT oid FROM pg_roles WHERE rolname=current_user),
		datallowconn FROM pg_database WHERE datname=$1`, db).Scan(&dbOID, &owned, &acceptingConnections)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	// A crash immediately after CREATE DATABASE leaves the reserved name sealed and without a recorded OID.
	if dbOID != 0 && (!owned || (r.dbOID != dbOID && (r.dbOID != 0 || acceptingConnections))) {
		return fmt.Errorf("database %q does not match its registry record", db)
	}

	role := lifecycle.RoleName(r.ID)
	var roleOID int64
	err = s.conn.QueryRow(ctx, "SELECT oid::bigint FROM pg_roles WHERE rolname=$1", role).Scan(&roleOID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if roleOID != 0 && roleOID != r.roleOID {
		return fmt.Errorf("role %q does not match its registry record", role)
	}

	if dbOID != 0 {
		if _, err := s.conn.Exec(ctx, "ALTER DATABASE "+ident(db)+" ALLOW_CONNECTIONS false"); err != nil {
			return err
		}
		if _, err := s.conn.Exec(ctx, "DROP DATABASE "+ident(db)+" WITH (FORCE)"); err != nil {
			return err
		}
	}
	if roleOID != 0 {
		if _, err := s.conn.Exec(ctx, "DROP ROLE "+ident(role)); err != nil {
			return err
		}
	}
	return nil
}
