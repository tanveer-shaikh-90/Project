#!/usr/bin/env python3
"""
On-sale stampede generator for the seat-reservation service.

Fires a burst of concurrent reservations at a live service and prints the
outcome distribution plus the final reconciliation invariant.

Scenarios exercised:
  1. Hot-seat storm  : N users all fight for the SAME seat -> exactly 1 win, rest 409.
  2. Idempotent retry: the same user + key fired many times -> exactly 1 reservation.
  3. Per-user limit  : one user fires more parallel reserves than the limit.
  4. Spread load     : many users across many distinct seats.

Usage:
    python scripts/burst.py <BASE_URL> [--seats 2000] [--hot-users 500] [--total 20000]

Only the Python stdlib + aiohttp are required:
    pip install aiohttp
"""
import argparse
import asyncio
import random
import string
import sys
import time
import uuid
from collections import Counter

try:
    import aiohttp
except ImportError:
    sys.exit("aiohttp is required: pip install aiohttp")

ADMIN_TOKEN = "admin-burst"


def rand_name(prefix: str) -> str:
    return prefix + "-" + "".join(random.choices(string.ascii_lowercase, k=6))


async def create_show(session: aiohttp.ClientSession, base: str, n_seats: int, price: int):
    seats = [f"A{i}" for i in range(1, n_seats + 1)]
    payload = {"name": rand_name("show"), "seats": seats, "price_paise": price, "per_user_limit": 4}
    async with session.post(
        f"{base}/shows",
        json=payload,
        headers={"Authorization": f"Bearer {ADMIN_TOKEN}"},
    ) as resp:
        if resp.status != 201:
            body = await resp.text()
            sys.exit(f"failed to create show: {resp.status} {body}")
        data = await resp.json()
        return data["id"], seats


async def reserve(session, base, show_id, user, seats, key):
    """Returns (http_status, reason) — reason classifies the decline."""
    headers = {"Authorization": f"Bearer {user}", "Idempotency-Key": key}
    body = {"seats": seats, "idempotency_key": key, "user_id": "spoofed-victim"}
    try:
        async with session.post(f"{base}/shows/{show_id}/reserve", json=body, headers=headers) as resp:
            status = resp.status
            reason = None
            if status not in (200, 201):
                try:
                    data = await resp.json()
                    reason = data.get("error")
                except Exception:
                    reason = "non_json"
            elif status == 200:
                reason = "idempotent_replay"
            return status, reason
    except Exception as exc:  # noqa: BLE001
        return 0, f"client_error:{type(exc).__name__}"


def classify(results):
    counter = Counter()
    for status, reason in results:
        if status == 201:
            counter["confirmed (201)"] += 1
        elif status == 200:
            counter["idempotent_replay (200)"] += 1
        elif status == 409:
            counter[f"declined-409 [{reason}]"] += 1
        elif 400 <= status < 500:
            counter[f"declined-{status} [{reason}]"] += 1
        elif status >= 500:
            counter[f"SERVER_ERROR-{status}"] += 1
        else:
            counter[f"client_error [{reason}]"] += 1
    return counter


def print_counter(title, counter):
    print(f"\n=== {title} ===")
    for key in sorted(counter):
        print(f"  {key:<45} {counter[key]}")


async def get_show(session, base, show_id):
    async with session.get(
        f"{base}/shows/{show_id}", headers={"Authorization": "Bearer observer"}
    ) as resp:
        return await resp.json()


async def hot_seat_storm(session, base, show_id, seat, n_users):
    """n_users all target the same seat with unique keys."""
    tasks = [
        reserve(session, base, show_id, f"user-{i}", [seat], uuid.uuid4().hex)
        for i in range(n_users)
    ]
    return await asyncio.gather(*tasks)


async def idempotent_retries(session, base, show_id, seat, n):
    """Same user + same key fired n times -> exactly one reservation."""
    user = "retry-user"
    key = uuid.uuid4().hex
    tasks = [reserve(session, base, show_id, user, [seat], key) for _ in range(n)]
    return await asyncio.gather(*tasks)


async def per_user_limit_blast(session, base, show_id, seats, n):
    """One user fires n parallel single-seat reserves; limit should cap holds at 4."""
    user = "greedy-user"
    tasks = [
        reserve(session, base, show_id, user, [seats[i % len(seats)]], uuid.uuid4().hex)
        for i in range(n)
    ]
    return await asyncio.gather(*tasks)


async def spread_load(session, base, show_id, seats, total):
    tasks = []
    for i in range(total):
        seat = random.choice(seats)
        tasks.append(reserve(session, base, show_id, f"user-{i}", [seat], uuid.uuid4().hex))
    return await asyncio.gather(*tasks)


async def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("base_url")
    parser.add_argument("--seats", type=int, default=2000)
    parser.add_argument("--hot-users", type=int, default=500)
    parser.add_argument("--total", type=int, default=20000)
    parser.add_argument("--price", type=int, default=25000)
    args = parser.parse_args()
    base = args.base_url.rstrip("/")

    connector = aiohttp.TCPConnector(limit=0)  # unbounded client concurrency
    timeout = aiohttp.ClientTimeout(total=120)
    async with aiohttp.ClientSession(connector=connector, timeout=timeout) as session:
        print(f"Target: {base}")
        show_id, seats = await create_show(session, base, args.seats, args.price)
        print(f"Created show {show_id} with {len(seats)} seats")

        overall = Counter()
        start = time.time()

        # 1. Hot-seat storm on A12.
        print(f"\n[1] Hot-seat storm: {args.hot_users} users fighting for seat 'A12'...")
        r = await hot_seat_storm(session, base, show_id, "A12", args.hot_users)
        c = classify(r)
        print_counter("Hot-seat storm", c)
        overall.update(c)
        confirmed = c.get("confirmed (201)", 0)
        print(f"  -> exactly-one-winner check: {'PASS' if confirmed == 1 else 'FAIL'} (confirmed={confirmed})")

        # 2. Idempotent retries on A13.
        print("\n[2] Idempotent retries: same user+key x500 on 'A13'...")
        r = await idempotent_retries(session, base, show_id, "A13", 500)
        c = classify(r)
        print_counter("Idempotent retries", c)
        overall.update(c)
        created = c.get("confirmed (201)", 0)
        print(f"  -> at-most-one-create check: {'PASS' if created <= 1 else 'FAIL'} (created={created})")

        # 3. Per-user limit blast.
        print("\n[3] Per-user limit: greedy-user fires 50 parallel reserves (limit=4)...")
        r = await per_user_limit_blast(session, base, show_id, seats[100:160], 50)
        c = classify(r)
        print_counter("Per-user limit", c)
        overall.update(c)
        got = c.get("confirmed (201)", 0)
        print(f"  -> limit-held check: {'PASS' if got <= 4 else 'FAIL'} (confirmed={got})")

        # 4. Spread load.
        print(f"\n[4] Spread load: {args.total} reservations across {len(seats)} seats...")
        r = await spread_load(session, base, show_id, seats, args.total)
        c = classify(r)
        print_counter("Spread load", c)
        overall.update(c)

        elapsed = time.time() - start

        print_counter("OVERALL OUTCOME DISTRIBUTION", overall)
        server_errors = sum(v for k, v in overall.items() if k.startswith("SERVER_ERROR"))
        print(f"\n  Total 5xx server errors: {server_errors}  ->  {'PASS' if server_errors == 0 else 'FAIL'}")
        print(f"  Wall clock: {elapsed:.1f}s")

        # Final reconciliation.
        show = await get_show(session, base, show_id)
        counts = show["counts"]
        total_seats = show["total_seats"]
        summed = counts["available"] + counts["held"] + counts["confirmed"]
        print("\n=== FINAL RECONCILIATION ===")
        print(f"  available={counts['available']} held={counts['held']} confirmed={counts['confirmed']}")
        print(f"  available+held+confirmed = {summed}  vs total_seats = {total_seats}")
        print(f"  reconciliation check: {'PASS' if summed == total_seats else 'FAIL'}")


if __name__ == "__main__":
    asyncio.run(main())
