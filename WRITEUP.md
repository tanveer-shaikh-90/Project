# Design and Operational Write-Up

## Atomic Decision

PostgreSQL READ COMMITTED transactions are the only write authority. `Reserve` acquires locks in this order:

1. Insert a `booking_users` row with `ON CONFLICT DO NOTHING`, then `SELECT ... FOR UPDATE` that user's row. Each transaction locks only one user. This serializes that user's requests across seats **and shows**.
2. Look up the user's idempotency key while holding the user lock.
3. Read the show's price/limit; lock requested seat rows with `ORDER BY label FOR UPDATE`.
4. Check every seat exists and is available, then count that user's currently confirmed seats for the show.
5. Insert one reservation, update all locked seats, and commit. Any unsuccessful transaction rolls back all mutations.

The user lock closes the limit race: ten requests for different seats cannot all read the same stale count. READ COMMITTED gives the next count query a fresh snapshot after the previous transaction commits. No process-local mutex is used, so the rule also holds across API replicas.

For a contested seat, only one transaction observes availability while holding its row lock. After it commits, waiting lock readers see the updated status and return `409 seat_taken`. `UNIQUE(show_id,label)` makes a seat one physical row. Foreign keys and ownership/status checks add structural protection. The seat check and update are in one transaction, not a read-then-write outside a lock.

## Multi-Seat and Cancellation Ordering

Requests are all-or-nothing. If any seat is missing or unavailable, none are sold. Input labels are validated and sorted; duplicates are rejected. PostgreSQL also orders the locking query by label, using the database's collation consistently in both paths.

Cancellation takes the calling user's mutex, then the reservation row, then all its seat rows **in the same label order**. It checks ownership before releasing anything. Reserve never takes a lock on another user's reservation row. A transaction takes only one user mutex, and all seat-lock acquisition follows one order, avoiding cycles among these operations. External SQL writers must respect the same rules.

Cancellation updates only rows still associated with that reservation and in confirmed state. Repeated cancellation returns the cancelled reservation without touching seats. A stale cancellation therefore cannot free a seat that has since been booked with a different reservation ID.

## Idempotency

The durable reservation row stores `(user_id,idempotency_key)` under a unique constraint and stores SHA-256 of canonical JSON containing show ID and sorted seats. JSON encoding prevents delimiter ambiguity between labels such as `A,B` and separate labels `A` and `B`.

The user mutex is acquired **before** lookup, so concurrent same-key requests wait for the first commit, then see the winner. The implementation does not attempt to query a transaction after a unique-violation error has aborted it. The unique constraint remains a defensive guard.

A successful first request returns 201. Matching retries return 200 with the original ID and current reservation status. A changed show or seat set returns 409. Keys are user-scoped across shows. Cancelled keys remain consumed and return `cancelled`, never a new booking. New purchases need new keys. Declined attempts are not persisted as idempotency outcomes and can be retried; keys become durable when a reservation commits.

If the database commits but the HTTP response is lost, retrying the same key recovers the result without another allocation. There is **no payment processor** in this exercise: exactly-once concerns the reservation and its integer-paise amount, not an unimplemented external charge. A real payment integration would need its own idempotency key, reconciliation, and transactional outbox.

## Holds and Identity

This implementation chooses immediate confirmation plus explicit owner cancellation. There is no TTL, sweeper, or separate payment confirmation stage; `held` is always zero. Cancellation releases both capacity and the owner's per-show allowance.

Identity comes only from a validated HS256 JWT subject. Tokens require the expected signing algorithm, issuer, audience, and expiry. A separate configured admin credential controls show creation. Body `user_id` fields have no effect. The public demo issuer allocates random subjects and never issues admin privileges. It does not prove a real person's identity or prevent multiple anonymous registrations; production would use a real identity provider and rate limiting.

## Reconciliation and Money

Every seat stays in one physical row and one status. `GET /shows/{id}` derives all seat counts from one seat-query MVCC snapshot. The burst compares counts against the originally created label set, checks unique labels, and reconciles final confirmed seats with successful reservation responses, rather than only checking a tautological sum.

Amounts are Go `int64` and PostgreSQL `BIGINT`, calculated only after checking multiplication overflow. Prices must be nonnegative integer JSON numbers and seat lists nonempty. Floating-point values are used only for telemetry such as latency and Prometheus exposition, never booking money.

## Partitions, Capacity, and Deployment

The design chooses consistency over availability. A partitioned API cannot bypass the PostgreSQL primary and confirm from memory. Readiness fails closed; database failures/timeouts remain real 5xx. The service does not turn infrastructure failures into misleading `seat_taken` declines.

The application pool is bounded at 25 connections by default. HTTP requests may wait for a connection; they have a 180-second deadline. This prevents unbounded transaction lifetime but does not promise unlimited capacity or zero errors during outages. Readiness has a two-second dependency budget and metrics a five-second budget. Pool saturation can trip these checks; measure and provision before the evaluator's burst.

Horizontal API replicas share one primary and the same signing/admin secrets. Budget total connections across replicas below the database's limit. A single hot seat is inherently serialized; adding replicas does not remove that contention. A bounded client test with 20,000 requests is not proof of 20,000 simultaneous socket support. The harness exposes its concurrency setting explicitly.

Startup schema changes use an advisory transaction lock plus `schema_migrations`. The schema and version record commit together, so multiple cold-starting replicas cannot race a partial migration. The database must already exist and the deployment user needs schema permissions. Future changes require a new migration version, not edits to a previously applied version.

Docker builds are checksum-backed, non-root at runtime, include CA certificates, and do not hard-code an x86 architecture. The CI pipeline builds AMD64 and ARM64 containers and exercises PostgreSQL-backed tests. A public host, its proxy limits, and its cold-start behavior still require a real deployment check.

## Observability and 2am Alerts

Structured JSON stdout logs carry a generated/validated request UUID, route, status, and duration. Bearer credentials and request bodies are not logged. Error responses echo the correlation ID. Provider log visibility is not assumed: supply a recording or redacted export if public live logs are unavailable.

Availability, per-state counts, totals, and lifetime confirmed reservations come from one database statement per metrics scrape. This prevents in-memory gauge drift after restart or across replicas. The confirmed counter includes subsequently cancelled reservations. A failed DB read fails the scrape instead of returning zero. The HTTP and decline counters are process-local and reset on restart, so use Prometheus rates. Replay is tracked under the requested decline-reason family even though its response is successful HTTP 200.

Page for reservation 5xx, sustained failed metrics/readiness, or any reconciliation discrepancy. Investigate high p99 latency and a rising limit/conflict rate. Shipped Prometheus rules provide evaluations, not notification delivery; an Alertmanager receiver still needs configuration. Do not sum database-backed snapshot metrics across replicas. API reads and scrapes at different instants may differ legitimately during load.

## Verification and Evidence

Go unit tests cover token validation, expired/forged tokens, JSON parsing, seat validation, and unambiguous hashing. PostgreSQL API tests cover hot-seat contention, concurrent idempotency and limits, opposite-order multi-seat requests, cancellation races, owner enforcement, schema reapplication, dependency health, and snapshot metrics after rebuilding the server object. The Python harness has its own negative tests and validates the deployed HTTP contract.

During this implementation, an isolated temporary Go compiler ran the available unit tests and compiled the database tests. PostgreSQL tests explicitly skipped because this laptop has no database or Docker runtime. GitHub Actions and the live deployment must supply the remaining runtime evidence. No live burst numbers or public URL are claimed here. Download genuine CI/live artifacts after running the workflows described in the README.

## AI Usage

GitHub Copilot generated the initial implementation and much of the corrected code, tests, load harness, configuration, and this documentation in response to the supplied assignment and Go/PostgreSQL blueprint. The initial draft had real defects: a per-user concurrency race, incorrect concurrent-idempotency handling, forgeable token authentication, and restart-unsafe gauges. The subsequent AI-assisted review identified and corrected these and added validation.

The user supplied the assignment, preferred blueprint, account/repository context, the Mac compatibility requirement, and the requirement for a complete HLD and runbook. Implementation details in this revision were largely proposed and written by the assistant, not independently demonstrated as the candidate's decisions. The candidate must review the transaction sequence, run the database/live tests, and understand the trade-offs before submission. This document does not claim that review or a deployment has happened.

## Next Steps

- Run CI and deploy, then retain authentic load results, metrics, and live logs for evaluation.
- Replace anonymous demo registration with production identity and abuse controls.
- Measure pool/lock wait, tune admission limits and readiness isolation, and establish a capacity budget.
- Add TTL checkout holds only with explicit confirm/expire state transitions and race tests.
- Introduce versioned retention policies for old users, reservations, and per-show metrics without prematurely discarding retry history.
- Add an outbox and payment reconciliation before connecting a real payment provider.