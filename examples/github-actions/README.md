# GitHub Actions preview lifecycle

Copy the four files in [workflows](workflows/) into your repository's `.github/workflows` folder. They assume that PRs target `main`, all shared-cluster previews come from trusted same-repository contributors, and the selected runner can reach your preview PostgreSQL instance and deployment API. Change the branch filters and runner labels to match your project.

Create the `dbbranch-preview` environment with the `DBBRANCH_ADMIN_URL` secret. Set repository variables `DBBRANCH_REPOSITORY` and `DBBRANCH_REF` to the manager's GitHub repository and a trusted commit SHA. Configure environment access for trusted PRs or use a separate ephemeral database for untrusted contributions. If your GitHub Actions policy restricts `pull_request_target`, allow the metadata-only closure workflow or use the scheduled/manual reconciliation workflow to clean up closed PRs.

## Project hooks

| Hook | Contract |
| --- | --- |
| `ci/build-preview-baseline.sh OUTFILE` | Set up a scratch PostgreSQL database, run the checked-out base revision's migrations, seed synthetic data, and write a custom-format `pg_dump -Fc` archive at `OUTFILE`. Include migration history. |
| `ci/migrate-preview.sh` | Use `DATABASE_URL` to run the project's existing migration command, such as `prisma migrate deploy`, `alembic upgrade head`, or `flyway migrate` with equivalent connection settings. |
| `ci/deploy-preview.sh PR REVISION GENERATION_ID` | Deploy the candidate with `DATABASE_URL`. Label the deployment with `GENERATION_ID` and wait for health checks. At cutover, confirm the PR is still open and the merge revision is current. If the deployment platform promotes asynchronously, require generation-aware conditional promotion so an outdated task cannot overwrite the active route. |
| `ci/preview-generation-unused.sh PR GENERATION_ID` | Return success only when no active, starting, or draining deployment can use the generation. Treat deployment API errors and ambiguous status as in-use. The reconciler runs this from the trusted default branch. |
| `ci/teardown-preview.sh PR` | Remove the preview route and stop every deployment for that PR. Return success when the deployments can no longer use PostgreSQL. The hook must be idempotent and runs from the trusted default branch. |

Trigger a baseline build for the current base revision before the first preview. The preview workflow uses the first parent of the tested merge commit to select the artifact. It runs the project's migration against a fresh generation on each workflow attempt. The existing preview remains available when a migration fails, and unused generations are reclaimed after checking deployment references.

Closure and reconciliation share the preview's per-PR concurrency group. GitHub.com's `queue: max` prevents routine reconciliation runs from replacing pending preview runs. The scheduled pass also reconciles missed closure events and retries abandoned resources. The default runner setup uses PostgreSQL 16 client tools, and the recovery matrix supports up to 256 registered PRs per run.
