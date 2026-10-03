# Seat Reservation at Scale

A small JSON HTTP service that sells **assigned seats** for an event and lets users
reserve them correctly under extreme concurrency. The whole point is correctness
under load: **never sell the same seat twice, never exceed a user's booking limit,
never double-charge a retried request** — even when tens of thousands of buyers
stampede the same show at on-sale time.

- Language: **Go** · Router: `chi` · Datastore: **PostgreSQL** (single DB) · Driver: `pgx`
- Money is integer **paise** (minor units), never floats.
- Observability: Prometheus metrics, structured JSON logs with request ids, liveness + readiness probes.

See [WRITEUP.md](WRITEUP.md) for the design rationale (the atomic decision, idempotency,
deadlock avoidance, CAP posture, and what we'd get paged for at 2am).

---

## Quick start (local, one command)

```bash
docker compose up --build -d      # or: make up
```

This starts Postgres 16 and the API on `http://localhost:8080`. The service runs
migrations on boot, so a clean checkout comes up ready.

Smoke test it:

```bash
./scripts/smoke.sh http://localhost:8080      # or: make smoke
```

Run the stampede against your running instance:

```bash
pip install aiohttp
python scripts/burst.py http://localhost:8080  # or: make burst BASE_URL=http://localhost:8080
```

---

## Authentication model

Identity is **derived strictly from the bearer token** — any `user_id` in the request
body is ignored. This is what makes the "act as another user" test impossible to pass.

- `Authorization: Bearer admin-<name>` → an **admin** (may create shows).
- `Authorization: Bearer <anything-else>` → a **user** whose id *is* the token.

A user can only cancel their own reservations.

> This is a deliberately simple, self-contained token scheme for the exercise. In
> production this would be a signed JWT validated against an auth service; the identity
> plumbing (token → `user_id`, never from the body) is identical.

---

## API

### Create a show — `POST /shows` (admin)

```bash
curl -X POST http://localhost:8080/shows \
  -H "Authorization: Bearer admin-karan" \
  -H "Content-Type: application/json" \
  -d '{"name":"friday-night","seats":["A1","A2","A3","A12"],"price_paise":25000,"per_user_limit":4}'
```

Returns `201` with the created show id and seat count; every seat starts `available`.

### Reserve seats — `POST /shows/{id}/reserve` (user)

```bash
curl -X POST http://localhost:8080/shows/$SHOW_ID/reserve \
  -H "Authorization: Bearer user-alice" \
  -H "Idempotency-Key: 11111111" \
  -H "Content-Type: application/json" \
  -d '{"seats":["A12"],"idempotency_key":"11111111"}'
```

Success `201`:

```json
{
  "reservation_id": "…",
  "show_id": "…",
  "user_id": "user-alice",
  "seats": ["A12"],
  "amount_paise": 25000,
  "status": "confirmed"
}
```

Behaviour:

| Situation | Result |
|---|---|
| Seat already held/confirmed by someone else | `409` `seat_taken` (clean decline, never `500`) |
| User would exceed `per_user_limit` (default 4) | `409` `per_user_limit` |
| Same idempotency key, same seats (retry) | `200` with the **original** reservation (replay) |
| Same idempotency key, **different** seats | `409` `idempotency_conflict` |
| Any requested seat unavailable (multi-seat) | **all-or-nothing**: whole request declines, nothing reserved |
| Seat label does not exist for the show | `404` `seat_not_found` |

The idempotency key may be sent as the `Idempotency-Key` header **or** the
`idempotency_key` body field (header wins).

### Cancel — `POST /reservations/{id}/cancel` (owner only)

```bash
curl -X POST http://localhost:8080/reservations/$RES_ID/cancel \
  -H "Authorization: Bearer user-alice"
```

Releases the reservation's seats back to `available`. Only the owner may cancel;
cancelling never resurrects a seat already confirmed to someone else.

### Show state — `GET /shows/{id}`

```bash
curl http://localhost:8080/shows/$SHOW_ID -H "Authorization: Bearer observer"
```

Returns every seat's status and reconciliation counts. The invariant
`available + held + confirmed == total_seats` holds at all times.

### Health & metrics

| Endpoint | Purpose |
|---|---|
| `GET /health/live` | liveness — process is up (`200`) |
| `GET /health/ready` | readiness — **pings Postgres**; `503` if the DB is down (fails closed) |
| `GET /metrics` | Prometheus exposition |

Key metrics:

- `reservations_confirmed_total` (counter)
- `reservations_declined_total{reason="seat_taken|per_user_limit|idempotency_conflict|idempotent_replay"}` (counter)
- `seats_available{show_id}` (gauge) — reconciles with `GET /shows/{id}`
- `http_requests_total{route,method,status}` and `http_request_duration_seconds` (histogram)

---

## The burst script

`scripts/burst.py` reproduces the on-sale stampede against any URL and prints the
outcome distribution + final reconciliation. It runs four scenarios:

1. **Hot-seat storm** — N users all fight for seat `A12` → asserts exactly one `201`, rest `409`.
2. **Idempotent retries** — same user+key fired 500× → asserts at most one reservation created.
3. **Per-user limit** — one greedy user fires 50 parallel reserves on a limit-4 show → asserts ≤ 4 confirmed.
4. **Spread load** — thousands of reservations across all seats.

```bash
python scripts/burst.py https://<your-live-url> --hot-users 500 --total 20000
```

Each scenario's body also carries a spoofed `"user_id":"spoofed-victim"` field to
prove identity is token-derived (it is ignored).

---

## Deploy to a public URL (Render free tier)

This repo includes [render.yaml](render.yaml) as a Render Blueprint.

1. Push this repo to GitHub.
2. Render → **New + → Blueprint** → select the repo.
3. Render provisions a free Postgres and builds the Docker image. `DATABASE_URL`
   is injected automatically; the service runs migrations on boot.
4. Health check path is `/health/ready` — Render only routes traffic once Postgres is reachable.
5. Grab the live URL and run `python scripts/burst.py <live-url>`.

Railway / Fly.io work the same way: provision Postgres, set `DATABASE_URL`, deploy the Dockerfile.

---

## Run the API locally without Docker

Needs Go 1.23+ and a reachable Postgres.

```bash
cp .env.example .env            # edit DATABASE_URL if needed
go mod tidy
go run ./cmd/api
```

## Project layout

```
seat-reservation/
├── cmd/api/main.go                 # entrypoint: config, DB, migrate, serve, graceful shutdown
├── internal/
│   ├── api/
│   │   ├── handlers.go             # routing + HTTP <-> domain mapping (201/200/409/404/400)
│   │   └── middleware.go           # token auth, request id, structured logs, metrics
│   ├── db/
│   │   ├── connection.go           # bounded pgx pool, migrate, readiness ping
│   │   └── migrations.sql          # tables, unique constraints, indexes
│   ├── metrics/metrics.go          # Prometheus collectors
│   └── service/reservation.go      # the atomic transaction logic
├── scripts/
│   ├── burst.py                    # one-command stampede + reconciliation
│   └── smoke.sh                    # quick end-to-end curl check
├── Dockerfile                      # multi-stage build -> small alpine image
├── docker-compose.yml              # Postgres + API for local dev
├── render.yaml                     # one-click Render deploy blueprint
└── WRITEUP.md
```
