# Cellar Run Control Specification

## Goal

Cellar Run Control is a local Go HTTP service for a small production cellar.
It coordinates approved formulas, vessel reservation, active fermentation
runs, gravity observations, conditioning, completion, and vessel release.

The project is a healthy initialization baseline. It contains no intentional
defects, no model traces, and no automated tests. Verification is intentionally
deferred to a later engineering task.

## Users

- Production planner: creates formulas, updates process steps, and approves a
  formula before it can be used.
- Cellar operator: registers vessels, creates runs, reserves a vessel, and
  starts fermentation.
- Fermentation observer: appends timestamped gravity observations and moves a
  run through conditioning and completion.
- Integration client: calls the stable JSON API from a local production script.

## Entities

### Formula

A formula contains an identifier, normalized name, style label, target original
gravity, target final gravity, maximum fermentation temperature, minimum and
maximum planned days, ordered process steps, lifecycle state, version, and
timestamps.

States are `draft`, `approved`, and `retired`.

- Names are unique after case folding and whitespace normalization.
- Target original gravity is in the inclusive range 1.030 to 1.120.
- Target final gravity is positive, lower than target original gravity, and
  differs from it by at least 0.005.
- Maximum fermentation temperature is in the inclusive range 5 to 40 Celsius.
- Planned duration is 1 to 120 days and the minimum cannot exceed the maximum.
- A formula has 1 to 24 process steps.
- Step sequence values must be contiguous and start at 1.
- Step temperatures are in the inclusive range 5 to 90 Celsius.
- Step hold times are 1 to 1440 minutes.
- `draft -> approved` requires a complete valid step list.
- An approved formula is immutable.
- `approved -> retired` is allowed only when no run currently uses it.

### Vessel

A vessel contains an identifier, unique display code, capacity in liters,
lifecycle state, active run identifier, version, and timestamps.

States are `available`, `reserved`, `in_use`, `cleaning`, and `retired`.

- Capacity is in the inclusive range 10 to 5000 liters.
- Only an available vessel can be reserved.
- A reserved or in-use vessel must identify exactly one active run.
- Completion moves an in-use vessel to cleaning.
- Cleaning can be finished only when the associated run is complete or
  aborted.
- Finishing cleaning releases the vessel to available.
- A vessel with an active run cannot be retired or directly reserved.

### FermentationRun

A run contains an identifier, unique run code, an approved formula identifier,
an optional vessel identifier, planned volume, notes, lifecycle state,
timetable, ordered observations, final metrics, version, and timestamps.

States are `planned`, `reserved`, `fermenting`, `conditioning`, `completed`,
and `aborted`.

- Planned volume is greater than zero and cannot exceed vessel capacity.
- Only an approved formula may be attached to a run.
- `planned -> reserved` atomically reserves an available vessel.
- `reserved -> fermenting` requires that the vessel still belongs to the run.
- `fermenting -> conditioning` requires at least two observations.
- `conditioning -> completed` requires at least three observations.
- Completion computes apparent attenuation and estimated alcohol by volume from
  the formula target original gravity and the last observation.
- Completion moves the vessel to cleaning without losing its run association.
- A non-completed run can be aborted. Aborting before fermentation releases an
  available vessel; aborting after fermentation schedules the vessel for
  cleaning.
- Completed and aborted runs are immutable.
- A run can carry at most 500 observations.

### GravityObservation

An observation contains a run identifier, contiguous sequence number, observed
time, gravity, temperature, optional note, and receipt time.

- Gravity is in the inclusive range 0.980 to 1.200.
- Temperature is in the inclusive range -5 to 60 Celsius.
- Observed time begins at or after the run start.
- Observation times strictly increase within a run.
- Gravity must not rise more than 0.010 above the immediately preceding value.
- Observation sequences are contiguous and start at 1.

## Connected Workflows

### 1. Formula Approval

Public entry points are:

- `POST /v1/formulas`
- `PUT /v1/formulas/{id}/steps`
- `POST /v1/formulas/{id}/approve`
- `GET /v1/formulas/{id}`

The client creates a draft, sets its process step list atomically, approves it,
and reads the approved record. Invalid ranges, duplicate names, empty steps,
and repeated approval return stable error codes without partial writes.

This path crosses HTTP protocol handling, application orchestration, domain
rules, and snapshot storage.

### 2. Vessel Run Lifecycle

Public entry points are:

- `POST /v1/vessels`
- `POST /v1/runs`
- `POST /v1/runs/{id}/reserve`
- `POST /v1/runs/{id}/start`
- `GET /v1/runs/{id}`

The client registers a vessel, creates a planned run against an approved
formula, reserves the vessel, starts fermentation, and reads the run. Capacity
conflicts, an unavailable vessel, duplicate run codes, and stale versions
produce stable failures without changing vessel or run state.

This path crosses HTTP protocol handling, application orchestration, domain
rules, storage coordination, and resource lifecycle.

### 3. Observations And Completion

Public entry points are:

- `POST /v1/runs/{id}/observations`
- `POST /v1/runs/{id}/conditioning`
- `POST /v1/runs/{id}/complete`
- `GET /v1/runs/{id}`

The client appends three ordered observations, starts conditioning, completes
the run, and reads the calculated result. Non-monotonic times, rising gravity,
out-of-range values, excessive sequences, incomplete data, and repeated
transitions return stable failures without changing persisted state.

This path crosses HTTP protocol handling, application orchestration, numeric
domain rules, and snapshot storage.

## Modules

- `cmd/fermentctl`: executable entry point for the HTTP service and bounded
  workflow checks.
- `internal/config`: environment and command-line configuration.
- `internal/clock`: deterministic clock boundary for production and checks.
- `internal/domain`: entities, validation, state transitions, and metrics.
- `internal/ratelimit`: local per-terminal, per-category write quotas and
  durable block records.
- `internal/service`: use-case orchestration and revision coordination.
- `internal/storage`: snapshot repository, atomic replacement, and locking.
- `internal/httpapi`: routing, strict JSON decoding, error mapping, quota
  gating, and health.
- `internal/workflowcheck`: real HTTP workflow checks used by the executable.

The dependency direction is:

`cmd -> workflowcheck/httpapi -> service -> domain`

`service -> storage`

`storage -> domain`

`httpapi -> ratelimit -> clock`

`config -> ratelimit -> clock`

The domain package does not import outer packages.

## API Behavior

All business routes are JSON under `/v1`. A successful write returns the
updated aggregate and its version. A failed write returns:

```json
{
  "error": {
    "code": "stable_machine_code",
    "detail": "human-readable explanation"
  }
}
```

Write requests may include `X-Expected-Version`. When present, the service
rejects a stale write instead of overwriting newer state.

`GET /healthz` returns process health and the current snapshot revision. It is
never gated by quotas.

### Local Write Quotas

Mutating `/v1` requests carry a local quota keyed by terminal and operation
category. Categories are `formula`, `vessel`, `run`, and `observation`
(appended gravity observations are counted separately from other run writes).

- A terminal is the `X-Cellar-Terminal` request header when well formed,
  otherwise the peer IP address.
- Each category allows a configured number of accepted writes in a trailing
  window. The write that exceeds the limit starts a fixed penalty block whose
  duration is at least the longest window.
- Evaluation happens before request-body decoding and before any business
  call, so a rejected write cannot touch business state or the serialized
  store. Every decision is made under one lock; bursts, requests crossing a
  window boundary, and concurrent runtime configuration changes each observe
  exactly one consistent outcome.
- A blocked write returns HTTP 429 with stable code `quota_exceeded`,
  `Retry-After` in seconds, `X-RateLimit-Limit`, `X-RateLimit-Remaining`, and
  `X-RateLimit-Reset`.
- `GET /healthz`, all read-only detail/list endpoints, and
  `GET /quota-status` are never gated. `/quota-status` reports per-category
  remaining allowance and an explainable active block without recording a
  request.
- Runtime rule changes come from an optional JSON quota file reloaded on
  `SIGHUP`; an invalid file is rejected and leaves the running rules in force.
  Raising a limit never removes an active penalty.
- Active blocks are persisted to `quota-blocks.json` in the data directory
  with the same atomic replacement protocol as the snapshot. Window history is
  not persisted. After a restart, still-active blocks are restored and marked
  as such in the response (`restart_restored: true`) and in `/quota-status`;
  expired blocks are pruned. A corrupt blocks file stops startup.
- Quotas can be disabled entirely with `CELLAR_CONTROL_QUOTA_ENABLED=false`.

## Persistence And Concurrency

The configured data directory contains `state.json`. Writes are serialized by
an in-process mutex. The complete state is encoded, committed to durable
storage, and then atomically renamed over the target. An update is not
published in memory unless persistence succeeds.

Reads use copied values and never expose repository-owned maps or slices. The
default address is `127.0.0.1:8088`, the request body limit is 1 MiB, and all
workflow checks use temporary directories.

## Validation Plan

The baseline declares `testing: deferred` and intentionally contains no unit
tests, fixtures, integration tests, or browser E2E files. A later verification
task owns systematic tests for validation boundaries, state transitions,
numeric calculations, stale revisions, storage failures, and concurrent
reservation attempts.

The current build command is `go build ./...`. Three bounded production checks
start the real HTTP handler, issue real JSON requests, check success and failure
behavior, and exit without external services:

- `go run ./cmd/fermentctl check formula-approval`
- `go run ./cmd/fermentctl check vessel-run-lifecycle`
- `go run ./cmd/fermentctl check observations-completion`
- `go run ./cmd/fermentctl check quota-enforcement`

The quota-enforcement check runs the real HTTP handler against a fixed clock
and covers burst rejection with an explicit wait, health/read exemption,
per-terminal and per-category isolation, sliding-window rollover, rejected
requests leaving business state unchanged, runtime limit adjustment, a
concurrent burst producing exactly one decision per request, and block
restoration after a controller restart.

## Scope Limits

This service does not control physical equipment, connect to a remote identity
provider, send notifications, expose a browser interface, replicate data, or
replace laboratory measurements. The API is intended for a trusted local
network or an internal adapter.
