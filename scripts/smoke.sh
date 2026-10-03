#!/usr/bin/env bash
# Quick end-to-end sanity check against a running service.
# Usage: ./scripts/smoke.sh [BASE_URL]
set -euo pipefail

BASE="${1:-http://localhost:8080}"
ADMIN="Authorization: Bearer admin-smoke"
USER="Authorization: Bearer user-alice"

echo "== liveness =="
curl -sf "$BASE/health/live"; echo

echo "== readiness =="
curl -sf "$BASE/health/ready"; echo

echo "== create show =="
SHOW=$(curl -sf -X POST "$BASE/shows" -H "$ADMIN" -H 'Content-Type: application/json' \
  -d '{"name":"friday-night","seats":["A1","A2","A3","A12"],"price_paise":25000}')
echo "$SHOW"
SHOW_ID=$(echo "$SHOW" | sed -E 's/.*"id":"([^"]+)".*/\1/')
echo "show_id=$SHOW_ID"

echo "== reserve A12 =="
KEY=$(date +%s%N)
curl -sf -X POST "$BASE/shows/$SHOW_ID/reserve" -H "$USER" -H 'Content-Type: application/json' \
  -d "{\"seats\":[\"A12\"],\"idempotency_key\":\"$KEY\"}"; echo

echo "== idempotent retry (same key) -> 200 replay =="
curl -s -o /dev/null -w "status=%{http_code}\n" -X POST "$BASE/shows/$SHOW_ID/reserve" \
  -H "$USER" -H 'Content-Type: application/json' \
  -d "{\"seats\":[\"A12\"],\"idempotency_key\":\"$KEY\"}"

echo "== second user grabs A12 -> 409 =="
curl -s -o /dev/null -w "status=%{http_code}\n" -X POST "$BASE/shows/$SHOW_ID/reserve" \
  -H "Authorization: Bearer user-bob" -H 'Content-Type: application/json' \
  -d "{\"seats\":[\"A12\"],\"idempotency_key\":$(date +%s%N)}"

echo "== show state =="
curl -sf "$BASE/shows/$SHOW_ID" -H "$USER"; echo

echo "smoke: done"
