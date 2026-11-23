package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"dbbranch/internal/lifecycle"
	"github.com/jackc/pgx/v5"
)

func (s *Store) Create(ctx context.Context, scope lifecycle.Scope, revision, baseline, attempt, path string) (lifecycle.Generation, error) {
	if !lifecycle.ValidToken(revision) || !lifecycle.ValidToken(baseline) || !lifecycle.ValidToken(attempt) {
		return lifecycle.Generation{}, errors.New("valid revision, baseline and attempt are required")
	}
	archive, err := os.Open(path)
	if err != nil {
		return lifecycle.Generation{}, fmt.Errorf("open baseline: %w", err)
	}
	defer archive.Close()
	info, err := archive.Stat()
	if err != nil {
		return lifecycle.Generation{}, err
	}
	if !info.Mode().IsRegular() {
		return lifecycle.Generation{}, errors.New("baseline must be a regular PostgreSQL custom archive file")
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, archive); err != nil {
		return lifecycle.Generation{}, err
	}
	if _, err := archive.Seek(0, io.SeekStart); err != nil {
		return lifecycle.Generation{}, err
	}
	digest := hex.EncodeToString(hash.Sum(nil))

	unlock, err := s.lock(ctx, scope)
	if err != nil {
		return lifecycle.Generation{}, err
	}
	defer unlock()

	existing, err := readRecord(s.conn.QueryRow(ctx, "SELECT "+columns+`
		FROM dbbranch.generations
		WHERE repo_id=$1 AND environment=$2 AND pr=$3 AND revision=$4 AND baseline=$5 AND attempt=$6`,
		scope.RepoID, scope.Env, scope.PR, revision, baseline, attempt))
	switch {
	case err == nil:
		if existing.digest != digest {
			return lifecycle.Generation{}, errors.New("baseline archive changed for the same generation")
		}
		if existing.State == lifecycle.Ready || existing.State == lifecycle.InUse {
			if err := s.checkResources(ctx, existing); err != nil {
				return lifecycle.Generation{}, err
			}
			return existing.Generation, nil
		}
		if err := s.delete(ctx, existing); err != nil {
			return lifecycle.Generation{}, fmt.Errorf("remove incomplete generation: %w", err)
		}
	case !errors.Is(err, pgx.ErrNoRows):
		return lifecycle.Generation{}, err
	}

	id, err := lifecycle.NewID()
	if err != nil {
		return lifecycle.Generation{}, err
	}
	r := record{
		Generation: lifecycle.Generation{Scope: scope, ID: id, Revision: revision, Baseline: baseline,
			Attempt: attempt, Database: lifecycle.DatabaseName(id), State: lifecycle.Provisioning},
		digest: digest, password: randomPassword(),
	}
	_, err = s.conn.Exec(ctx, `INSERT INTO dbbranch.generations
		(repo_id, environment, pr, id, revision, baseline, attempt, state, digest, password)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
		r.RepoID, r.Env, r.PR, r.ID, r.Revision, r.Baseline, r.Attempt, r.State, r.digest, r.password)
	if err != nil {
		return lifecycle.Generation{}, err
	}
	if err := s.provision(ctx, r, archive); err != nil {
		pending, lookupErr := s.byID(ctx, scope, id)
		if lookupErr != nil {
			return lifecycle.Generation{}, errors.Join(err, lookupErr)
		}
		return lifecycle.Generation{}, errors.Join(err, s.delete(ctx, pending))
	}
	r, err = s.byID(ctx, scope, id)
	return r.Generation, err
}

func (s *Store) provision(ctx context.Context, r record, archive *os.File) error {
	if err := s.createRole(ctx, r); err != nil {
		return fmt.Errorf("create preview role: %w", err)
	}
	if err := s.checkIsolation(ctx, r); err != nil {
		return err
	}
	if err := s.createDB(ctx, r); err != nil {
		return fmt.Errorf("create preview database: %w", err)
	}
	if err := s.restore(ctx, r, archive); err != nil {
		return err
	}
	return s.state(ctx, r.ID, lifecycle.Ready)
}

func (s *Store) Connection(ctx context.Context, scope lifecycle.Scope, id string) (string, error) {
	if !lifecycle.ValidID(id) {
		return "", errors.New("a managed generation ID is required")
	}
	unlock, err := s.lock(ctx, scope)
	if err != nil {
		return "", err
	}
	defer unlock()
	r, err := s.byID(ctx, scope, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", errors.New("generation not found in this PR")
	}
	if err != nil {
		return "", err
	}
	if r.State != lifecycle.Ready && r.State != lifecycle.InUse {
		return "", errors.New("generation is not ready")
	}
	if err := s.checkResources(ctx, r); err != nil {
		return "", err
	}
	if err := s.checkIsolation(ctx, r); err != nil {
		return "", err
	}
	if err := s.state(ctx, id, lifecycle.InUse); err != nil {
		return "", err
	}
	endpoint := s.clientURL(r, true)
	return endpoint.String(), nil
}

func (s *Store) Destroy(ctx context.Context, scope lifecycle.Scope, id string) error {
	if id != "" && !lifecycle.ValidID(id) {
		return errors.New("invalid generation ID")
	}
	unlock, err := s.lock(ctx, scope)
	if err != nil {
		return err
	}
	defer unlock()
	records, err := s.list(ctx, scope.RepoID, scope.Env, scope.PR)
	if err != nil {
		return err
	}
	var failures error
	for _, r := range records {
		if id == "" || r.ID == id {
			failures = errors.Join(failures, s.delete(ctx, r))
		}
	}
	return failures
}

func (s *Store) delete(ctx context.Context, r record) error {
	if err := s.state(ctx, r.ID, lifecycle.Deleting); err != nil {
		return err
	}
	if err := s.removeResources(ctx, r); err != nil {
		return fmt.Errorf("delete generation %s: %w", r.ID, err)
	}
	_, err := s.conn.Exec(ctx, "DELETE FROM dbbranch.generations WHERE id=$1", r.ID)
	return err
}

func (s *Store) GC(ctx context.Context, repoID int64, env string, cutoff time.Time, dryRun bool) ([]lifecycle.Generation, error) {
	records, err := s.list(ctx, repoID, env, 0)
	if err != nil {
		return nil, err
	}
	result := make([]lifecycle.Generation, 0)
	var failures error
	for _, candidate := range records {
		if !candidate.Collectible(cutoff) {
			continue
		}
		if dryRun {
			result = append(result, candidate.Generation)
			continue
		}
		r, removed, err := s.collect(ctx, candidate, cutoff)
		failures = errors.Join(failures, err)
		if removed {
			result = append(result, r.Generation)
		}
	}
	return result, failures
}

func (s *Store) collect(ctx context.Context, candidate record, cutoff time.Time) (record, bool, error) {
	unlock, err := s.lock(ctx, candidate.Scope)
	if err != nil {
		return record{}, false, err
	}
	defer unlock()
	r, err := s.byID(ctx, candidate.Scope, candidate.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		return record{}, false, nil
	}
	if err != nil || !r.Collectible(cutoff) {
		return record{}, false, err
	}
	if err := s.delete(ctx, r); err != nil {
		return record{}, false, err
	}
	return r, true, nil
}
