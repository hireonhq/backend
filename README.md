# backend-hireon-go

Go rewrite of the HireOn.AI backend. Replaces the Hapi/Mongoose service and moves
the database from MongoDB to Postgres.

## Why

Five defects in the previous backend could not be fixed without restructuring it,
and three of them were visible to users:

| Defect | Impact |
| --- | --- |
| `/jobs` sorted on `createdAt` alone, with 101 distinct timestamps across 2697 rows | Paging repeated jobs. 10 pages x 10 returned **56 unique jobs and 44 duplicates**; walking all pages **missed 1-9 rows** per filter |
| `GET /jobs/{jobId}` dereferenced the job before checking it existed | A valid but absent id returned **500 instead of 404** |
| CV upload deleted the previous file *before* uploading the replacement, then called the ML service inline with **no timeout anywhere** | A failed upload destroyed the user's CV; a slow ML service hung the request |
| CV embedding ran in a bare goroutine with no persisted record | A deploy, crash or OOM silently discarded in-flight embeddings, with no retry |
| `categories` was `[String]` while the seeder wrote ObjectIds, and the text index referenced two fields that did not exist | No referential integrity; the declared index was useless |

## Data migration

`tools/dump-mongo` + `cmd/import` move the data while **preserving the original
ObjectId hex ids**. That matters because user bookmarks and the ML service's CV
embeddings are keyed by those ids, so reminting them would silently orphan both.

The old `npm run seed` called `deleteMany()` and let `insertMany()` mint fresh ids
on every run, and it had no environment guard -- running it against production
would have destroyed every user's bookmarks. Import here is upsert-only.

## Layout

```
cmd/api            HTTP entry point, wiring, worker pool, graceful shutdown
cmd/import         Mongo dump -> Postgres loader (idempotent)
cmd/verify-import  asserts the migration preserved counts and relations
internal/config    env parsing with fail-fast validation
internal/domain    core types and sentinel errors (no HTTP, no SQL)
internal/httpx     response envelope, error mapping, middleware
internal/handler   request validation and response shaping
internal/service   business logic; imports no net/http
internal/repository/postgres  SQL, including the work queue
internal/worker   background job pool (claim/retry/recover)
internal/auth      Supabase JWKS verification
internal/storage   S3-compatible object storage (Cloudflare R2)
migrations         schema DDL
tools/compare.py   Node-vs-Go behavioural comparison
```

`internal/service` never imports `net/http`. In the Node backend a service
function referenced the Hapi response toolkit `h` that it was never passed -- the
kind of coupling this separation prevents.

## Setup

```bash
cp .env.example .env      # fill in the values
createdb hireon && psql hireon -f migrations/0001_init.sql
go run ./cmd/import ./dump
go run ./cmd/verify-import
go run ./cmd/api
```

## Background work

CV embedding is not done during the request. The upload stores the file, marks the
CV `pending`, writes a row to `jobs_queue`, and returns. A worker pool claims rows
with `FOR UPDATE SKIP LOCKED`, so:

- **work survives a restart** — the row is the source of truth, not process memory
- **two replicas never process the same job**, and no worker blocks another
- **failures retry** up to `MaxAttempts`, then stay `failed` for inspection
- **stranded jobs recover**: anything left `running` by a killed process is
  requeued at startup (`RequeueStale`)

Clients observe progress with `GET /profile/jobs/{jobId}`, which reports
`pending` / `running` / `done` / `failed` plus the result.

The first iteration of this service used a bare goroutine per upload. The unit
tests in `internal/worker` pin the behaviours that makes durable: retry until
success, stop at `MaxAttempts`, fail fast on an unknown job kind, requeue on
startup, and `Stop` waiting for in-flight work.

## Tests

```bash
go test ./...                                            # unit tests
TEST_DATABASE_URL=postgres://... go test -race ./...     # adds integration tests
python3 tools/compare.py http://node-host:3000 http://localhost:3000
```

`tools/compare.py` compares this service against a running Node backend. It
distinguishes structural equality (which must match exactly -- envelope shape,
pagination metadata, per-job field values, status codes) from set equality on
`/jobs`, where contents legitimately differ because the ordering bug is fixed.

## Behaviour changes

Deliberate, and each is covered by a test:

| Change | Reason |
| --- | --- |
| `/jobs` orders by `created_at DESC, id DESC` | Makes pagination complete and duplicate-free |
| Missing or invalid token returns **401** | Node returned 400 for an absent header and 403 for a bad optional token, so no client could handle "unauthenticated" uniformly |
| Absent job id returns **404** | Node returned 500 |
| Empty reference collections return `200 []` | Node returned 500 |
| CV and cover-letter work is queued, not inline | Removes the unbounded wait on the ML service |
| Object storage is not made public | Node called `makePublic()` on every upload, giving each user's CV a permanent unauthenticated URL |
| Preserved: `skilIdentificationDict` (typo), `scoreMatch` format, all response keys | The frontend depends on them |

## Container

A four-stage build: `deps` (module cache) -> `build` (static binary) -> `test`
(gate) -> `runtime` (distroless, non-root, no shell). The binary is ~28 MB and the
runtime image excludes the toolchain.

```bash
docker build --target runtime -t hireon-backend-go .
docker build --target test .          # run the suite inside the build
```

The test stage means a failing suite stops the build before a runtime image is
produced. `build.yml` runs the suite on a real pgvector Postgres first, then
publishes to GHCR — deployment stays off the runner because the host is only
reachable over a private network, matching the frontend pipeline.

## Configuration

Validated at startup; the service refuses to boot with any missing. Node read
`process.env` raw and logged a warning when `SUPABASE_URL` was unset, then failed
on every authenticated request.

| Variable | Notes |
| --- | --- |
| `DATABASE_URL` | Postgres connection string |
| `SUPABASE_URL` | JWKS is fetched from `<url>/auth/v1/.well-known/jwks.json` |
| `ML_SERVICE_URL`, `ML_SERVICE_API_KEY` | Recommendation and embedding calls |
| `S3_ENDPOINT`, `S3_REGION`, `S3_BUCKET`, `S3_ACCESS_KEY_ID`, `S3_SECRET_ACCESS_KEY` | Cloudflare R2 or any S3-compatible store |
| `S3_PUBLIC_BASE_URL` | Optional; public base for generated object URLs |
| `ML_TIMEOUT_SECONDS` | Default 30 |
