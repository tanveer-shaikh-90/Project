# WRITEUP — Seat Reservation at Scale

## The atomic decision (why it's race-free)

The decision "does this user get seat A12?" lives in **a single PostgreSQL
transaction holding row locks on the exact seats requested**. The flow in
`service.Reserve` is:

```
BEGIN;
  -- idempotency fast-path (see below)
  SELECT price_paise, per_user_limit FROM shows WHERE id = $show;
  SELECT label, status FROM seats
    WHERE show_id = $show AND label = ANY($seats)
    ORDER BY label
    FOR UPDATE;                         -- (1) lock the contended rows
  -- reject if any seat missing or not 'available'  (all-or-nothing)
  -- enforce per-user limit against currently held seats
  INSERT INTO reservations (... UNIQUE(user_id, idempotency_key) ...);
  UPDATE seats SET status='confirmed', held_by=$user, reservation_id=$rid
    WHERE show_id=$show AND label = ANY($seats);
COMMIT;
```

The key is `SELECT ... FOR UPDATE`. When 500 requests target seat `A12`
simultaneously, Postgres grants the row lock to exactly one transaction; the other
499 **block** on that same row until the winner commits. They then re-read the row,
see `status='confirmed'`, and return a clean `409 seat_taken`. There is no window
between "check" and "take" because both happen under the same lock — the classic
read-then-write race (`is A12 free? ok, take it`) is eliminated.

A unique seat is also structurally enforced: `seats` has `UNIQUE (show_id, label)`,
so a seat is a single physical row that can only be in one state.

### Multi-seat requests and deadlock avoidance

For `["A12","A13"]` we must lock multiple rows. Two concurrent requests for
`[A12, A13]` and `[A13, A12]` could deadlock if they grabbed locks in opposite
order. We prevent this by **imposing a total order on lock acquisition**: the
requested seats are de-duplicated and **sorted** (`dedupeSorted`), and the lock
query uses `ORDER BY label FOR UPDATE`. Every transaction therefore acquires seat
locks in the same deterministic order, so a cycle can never form — no deadlocks.

### Partial requests — our documented behaviour

**All-or-nothing.** If a user asks for `["A12","A13"]` and only one is free, the
entire request is declined with `409` and **nothing** is reserved. Because the
check and the mutation are inside the locked transaction, this holds under
concurrency: a seat cannot slip from available to taken between the check and the
commit. We chose all-or-nothing because partial fulfilment of an assigned-seat
purchase (e.g. a couple ending up in non-adjacent single seats) is usually worse
than a clean retry.

## Idempotency — exactly once

- **Where the key is stored:** the `reservations` table has
  `UNIQUE (user_id, idempotency_key)`, and we store a `request_hash`
  (`sha256(show_id + sorted_seats)`) alongside it.
- **How exactly-once is enforced:** on reserve we first look up `(user_id, key)`.
  If present and the hash matches, we return the **original** reservation as a
  replay (`200`). If absent, we attempt the `INSERT`. Under a concurrent same-key
  burst, two requests can both pass the initial lookup, but only one `INSERT` wins
  — the other hits a `23505` unique violation, which we catch and convert into a
  replay by re-reading the winner's row. The unique constraint, not application
  logic, is the source of truth, so it is correct even with no coordination.
- **Same key, different body:** if the stored `request_hash` differs from the
  current request's hash, we return `409 idempotency_conflict`. A key is a promise
  about one specific request; reusing it for different seats is a client bug we
  surface loudly rather than silently papering over.

## Holds & expiry — our model

We use an **explicit, owner-only cancel** model rather than time-boxed auto-expiry.
`POST /reservations/{id}/cancel` releases a reservation's seats back to
`available` inside a transaction that locks the reservation row and only touches
seats whose `reservation_id` still matches and whose status is still `confirmed`.
This guarantees a release can **never resurrect a seat already confirmed to someone
else** (a re-booked seat has a different `reservation_id`, so the cancel's
`UPDATE ... WHERE reservation_id = $old` affects zero of those rows).

The schema keeps a `held` seat state and `updated_at` so a TTL-hold variant (a
background sweeper that flips expired `held` rows back to `available`) is a small,
localized extension — but for this exercise the explicit-cancel model is simpler to
reason about and has no clock-skew edge cases.

## The reconciliation invariant

`available + held + confirmed == total_seats` holds continuously because every
state transition is a single `UPDATE` inside a transaction — a seat is never in
two states and never vanishes. `GET /shows/{id}` returns these counts, and the
`seats_available{show_id}` gauge is adjusted in the same logical operation
(confirm → `-n`, cancel → `+released`, create → `+N`). `scripts/burst.py` checks
the invariant after the storm.

## Consistency vs availability under a partition (CAP)

This service deliberately chooses **CP**: correctness over availability. The
atomic decision depends on a single Postgres primary. If the service cannot reach
the database it returns `503` from `/health/ready` (fails closed) and declines
writes rather than guessing — selling the same seat twice is far worse than being
briefly unavailable. We do **not** add a second write path or cache that could
confirm a seat without the DB's agreement.

Scaling is horizontal at the **stateless API** tier (many replicas behind one
Postgres); contention is absorbed by row locks, and the pool is deliberately
bounded (`DB_MAX_CONNS=25`) so a stampede is serialized at the hot row rather than
exhausting connections. If one DB primary became the ceiling, the next step is
partitioning by `show_id` (each show is an independent contention domain) across
primaries — seats never span shows, so no cross-shard transaction is needed.

## Observability — what I'd get paged for at 2am

- **Any `5xx`** on the reserve path. Declines are `4xx` domain outcomes; a `500`
  means a bug or a dependency failure. Page on
  `rate(http_requests_total{route="POST /shows/{id}/reserve",status=~"5.."}[1m]) > 0`.
- **Readiness flapping** (`/health/ready` returning `503`) → Postgres
  connectivity; the single most important dependency.
- **Reconciliation drift** — an alert comparing `seats_available{show_id}` against
  `GET /shows/{id}` counts; any divergence means a logic bug and is sev-1.
- **Latency p99 on reserve** climbing → lock contention or pool exhaustion;
  `http_request_duration_seconds` histogram is bucketed per route.
- **Decline-reason mix** — a spike in `per_user_limit` or `idempotency_conflict`
  can indicate a buggy client or an attack; `reservations_declined_total{reason}`
  breaks it down.

Every request carries a correlation id (`X-Request-ID`, generated if absent) that
appears in structured JSON logs and is echoed in responses and error bodies, so a
single booking can be traced end-to-end.

## AI usage — directed vs decided

AI tools were used and disclosed honestly.

- **Decided (mine):** the core correctness design — pushing the decision into a
  single transaction, choosing `SELECT ... FOR UPDATE` over advisory locks or
  optimistic retries, enforcing idempotency with a DB unique constraint plus a
  request hash, sorting seats for deterministic lock order, the all-or-nothing
  partial-request policy, the CP posture, and the explicit-cancel hold model.
- **Directed (AI-assisted):** scaffolding the repository layout, writing the
  boilerplate (router wiring, Prometheus plumbing, the `aiohttp` burst harness,
  Dockerfile and compose), and drafting this document from my bullet points. I
  reviewed and own every line; the concurrency decisions are genuinely mine and I
  can extend them live.

## What I'd do next

- **TTL holds**: add a reserve-then-confirm flow with a sweeper that expires
  `held` seats, for a real checkout funnel (the schema already supports it).
- **Load-shedding**: a concurrency limiter / queue in front of hot shows so the DB
  sees a bounded arrival rate under a true 20k-in-one-second spike.
- **Tests**: a concurrent Go test that fires N goroutines at one seat and asserts
  exactly one `201`, plus a property test for the reconciliation invariant.
- **Sharding by `show_id`** when a single primary becomes the bottleneck.
- **Grafana dashboard + alert rules** shipped with the repo so the observability is
  turnkey, not just exposed.
