# PostgreSQL Preview Databases Implementation Plan

**Goal** — Give trusted PR previews their own disposable PostgreSQL databases through the same CLI used in CI and locally.

**Architecture** — Go CLI, a PostgreSQL registry using session advisory locks, per-generation login roles, and logical restore from a trusted custom archive. GitHub Actions owns migrations and deployment.

**Spec** — The approved PostgreSQL and GitHub Actions MVP. The preview cluster is non-production and the management connection is direct.

- [x] Test and implement scope validation, generation identifiers, and the expiry policy in `internal/lifecycle`. Verify repository/environment separation and that exported connections protect a generation from expiry.
- [x] Test and implement the PostgreSQL registry and provider in `internal/provider/postgres`. Verify repeated concurrent creates, failure recovery, cross-database permission checks, independent migration histories, and idempotent cleanup against PostgreSQL 16.
- [x] Implement `create`, `status`, `connection`, `destroy`, and `gc` in `cmd/dbbranch`. Credentials are written to a new mode-0600 file only when requested. A generation becomes protected before credentials leave the manager.
- [x] Document local setup and wire trusted baseline/preview/cleanup GitHub Actions examples. Use a fresh generation for an intentional deployment retry and keep the previous database until its deployment is gone.
- [x] Verify the Go build, unit and PostgreSQL integration tests, and GitHub Actions examples.
