# Cellar Run Control

Cellar Run Control is a local Go HTTP service for coordinating a small
production cellar. It keeps formula approval, vessel availability, active
fermentation runs, gravity observations, conditioning, completion metrics, and
vessel cleaning in one deterministic process.

This repository is a healthy initialization baseline. It has no intentional
defects and no automated test suite. Tests are intentionally deferred to a
later engineering task; the production workflow checks in this repository
remain available for immediate build and behavior verification.

## Requirements

- Go 1.23 or later
- A writable local data directory
- No database, network service, or third-party module

## Build And Run

Build every package:

```bash
go build ./...
```

Start the service:

```bash
go run ./cmd/fermentctl serve
```

The default address is `127.0.0.1:8088`. The default snapshot directory is
`./data`.

Environment variables:

- `CELLAR_CONTROL_ADDR`: HTTP listen address.
- `CELLAR_CONTROL_DATA_DIR`: directory containing `state.json`.
- `CELLAR_CONTROL_QUOTA_ENABLED`: `true` (default) or `false`. When disabled no
  request is gated and `/quota-status` is not registered.
- `CELLAR_CONTROL_QUOTA_WINDOW`: trailing window shared by every category, as a
  Go duration (default `1m`).
- `CELLAR_CONTROL_QUOTA_PENALTY`: blocked duration after a burst (default
  `2m`, must be at least the window).
- `CELLAR_CONTROL_QUOTA_LIMIT_FORMULA`, `CELLAR_CONTROL_QUOTA_LIMIT_VESSEL`,
  `CELLAR_CONTROL_QUOTA_LIMIT_RUN`, `CELLAR_CONTROL_QUOTA_LIMIT_OBSERVATION`:
  accepted writes per window per terminal (defaults 30, 30, 30, 60).
- `CELLAR_CONTROL_QUOTA_FILE`: optional JSON file that overrides all
  environment-derived quota rules and is re-read on `SIGHUP`.

Quota file example:

```json
{
  "penalty": "2m",
  "rules": {
    "formula":     {"limit": 30, "window": "1m"},
    "vessel":      {"limit": 30, "window": "1m"},
    "run":         {"limit": 30, "window": "1m"},
    "observation": {"limit": 60, "window": "1m"}
  }
}
```

A running controller re-reads the quota file when it receives `SIGHUP`; an
invalid file is logged and rejected while the running rules stay in force.

Example:

```bash
CELLAR_CONTROL_ADDR=127.0.0.1:9099 \
CELLAR_CONTROL_DATA_DIR=/tmp/cellar-control \
go run ./cmd/fermentctl serve
```

## Local Write Quotas

Mutating `/v1` requests are gated by a local quota keyed on cellar terminal and
operation category (`formula`, `vessel`, `run`, `observation`). This protects
the single-machine service when a tablet is refreshed repeatedly or a script
hammers an endpoint: excess writes are rejected immediately instead of queuing
against the serialized business store.

A terminal is identified by the trusted local header `X-Cellar-Terminal`
(letters, digits, `._:-`, 64 characters); when it is absent the peer IP is
used. A sliding window allows at most the configured number of accepted
writes; the request that would exceed it starts a fixed penalty block for that
terminal/category. Bursts, window rollover, and runtime rule changes are
evaluated under one lock, so every request observes exactly one outcome.

Rejected writes never reach JSON decoding or business logic. They return
`429 Too Many Requests` with an explicit wait, and never modify state:

```json
{
  "error": {
    "code": "quota_exceeded",
    "detail": "write quota for formula operations from terminal \"tablet-7\" was exceeded; wait 120 second(s) before retrying"
  },
  "terminal": "tablet-7",
  "category": "formula",
  "retry_after_seconds": 120,
  "blocked_until": "2026-10-05T09:42:00Z",
  "restart_restored": false
}
```

Responses also carry `Retry-After` (seconds), `X-RateLimit-Limit`,
`X-RateLimit-Remaining`, and `X-RateLimit-Reset`. `GET /healthz`, every
read-only detail or list, and `GET /quota-status` are never gated, so
operators can always inspect the situation from the blocked terminal.

```bash
curl -H 'X-Cellar-Terminal: tablet-7' http://127.0.0.1:8088/quota-status
```

Active blocks are atomically persisted to
`<data-directory>/quota-blocks.json`. Sliding-window history is not persisted.
After a controller restart, blocks whose penalty is still active keep being
enforced and are reported with `restart_restored: true`; expired blocks are
pruned at startup. Raising limits at runtime never erases an active penalty.


## Directory Structure

```text
cmd/fermentctl                 service and workflow-check executable
internal/clock                 time boundary
internal/config                environment and quota-file configuration
internal/domain                entities, validation, transitions, metrics
internal/httpapi               HTTP routes, JSON decoding, errors, quota gate
internal/ratelimit             per-terminal per-category windows and blocks
internal/service               use-case orchestration
internal/storage               revisioned snapshot persistence
internal/workflowcheck         real HTTP production checks
```

## Public API

Health and quota introspection:

- `GET /healthz`
- `GET /quota-status` (requires enabled quotas)

Formula workflow:

- `POST /v1/formulas`
- `GET /v1/formulas`
- `GET /v1/formulas/{id}`
- `PUT /v1/formulas/{id}/steps`
- `POST /v1/formulas/{id}/approve`
- `POST /v1/formulas/{id}/retire`

Vessel workflow:

- `POST /v1/vessels`
- `GET /v1/vessels`
- `GET /v1/vessels/{id}`
- `POST /v1/vessels/{id}/finish-cleaning`
- `POST /v1/vessels/{id}/retire`

Run workflow:

- `POST /v1/runs`
- `GET /v1/runs`
- `GET /v1/runs/{id}`
- `POST /v1/runs/{id}/reserve`
- `POST /v1/runs/{id}/start`
- `POST /v1/runs/{id}/observations`
- `POST /v1/runs/{id}/conditioning`
- `POST /v1/runs/{id}/complete`
- `POST /v1/runs/{id}/abort`

Writes accept an optional `X-Expected-Version` header. Errors use a stable
shape:

```json
{
  "error": {
    "code": "revision_conflict",
    "detail": "the aggregate version does not match X-Expected-Version",
    "field": ""
  }
}
```

## Workflow Checks

Run each production workflow check separately:

```bash
go run ./cmd/fermentctl check formula-approval
go run ./cmd/fermentctl check vessel-run-lifecycle
go run ./cmd/fermentctl check observations-completion
go run ./cmd/fermentctl check quota-enforcement
```

Each check creates a temporary snapshot directory, starts the real HTTP
handler on a loopback listener, issues JSON requests, verifies success and
failure behavior, and removes its temporary data.

## Persistence And Recovery

The service stores a complete revisioned snapshot at
`<data-directory>/state.json`. A write creates a temporary file in the same
directory, commits the encoded bytes durably, closes the handle, and renames
the result over the previous snapshot. The in-memory snapshot is replaced only
after that operation succeeds.

On startup, an existing snapshot is decoded and normalized. Unknown or
conflicting map indexes are not silently invented; invalid JSON or a negative
revision stops the process before HTTP serving begins.

Active quota blocks use the same temp-file, sync, and atomic-rename protocol at
`<data-directory>/quota-blocks.json`. A corrupt blocks file (bad schema,
unknown category, inverted window) stops the process before serving; expired
entries are pruned during load.

## Repair-Ready Boundaries

The baseline intentionally keeps several independent behavior surfaces visible
for later engineering evaluation:

- formula name normalization and duplicate detection;
- target gravity and process-step range validation;
- aggregate version conflict checks;
- atomic vessel reservation and release;
- run state transition guards;
- observation sequence and time ordering;
- gravity movement and range checks;
- metric calculation and rounding;
- durable snapshot replacement and startup normalization.

These are production mechanisms, not injected defects. A later task can add
automated tests or request a focused repair without changing the public API.

## Test Boundary

This stage does not create unit tests, integration tests, test fixtures, or an
E2E suite. The later test task owns systematic coverage of validation limits,
state transitions, concurrency, persistence failures, and numeric edge cases.

## Scope

The service does not control physical equipment, authenticate remote users,
send notifications, expose a browser interface, or replicate data across
processes. It is intended for a trusted local network or an internal adapter.
