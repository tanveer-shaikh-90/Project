#!/usr/bin/env python3
"""Exercise a live API and exit nonzero on contract or transport failures."""

import argparse
import asyncio
import json
import os
import random
import sys
import time
import uuid
from collections import Counter

import aiohttp
from prometheus_client.parser import text_string_to_metric_families


def reconcile(show, expected_labels):
    seats = show["seats"]
    labels = [seat["label"] for seat in seats]
    counts = Counter(seat["status"] for seat in seats)
    assert len(labels) == len(set(labels)) == len(expected_labels), "seat count changed"
    assert set(labels) == set(expected_labels), "seat labels changed"
    assert set(counts) <= {"available", "held", "confirmed"}, "unknown state"
    assert show["total_seats"] == len(expected_labels), "incorrect total_seats"
    for state in ("available", "held", "confirmed"):
        assert counts[state] == show["counts"][state], f"incorrect {state} count"
    assert sum(show["counts"].values()) == len(expected_labels), "reconciliation drift"


def distribution(results):
    return Counter(
        "confirmed" if result[0] == 201 else
        "idempotent_replay" if result[0] == 200 else
        f"{result[0]}:{result[1].get('error', 'unknown')}"
        for result in results
    )


class Burst:
    def __init__(self, session, base, admin):
        self.session = session
        self.base = base
        self.admin = admin
        self.outcomes = Counter()
        self.http_statuses = Counter()
        self.latencies = []
        self.created = {}
        self.checks = []

    async def request(self, method, path, token=None, body=None):
        headers = {"Authorization": f"Bearer {token}"} if token else {}
        start = time.monotonic()
        try:
            async with self.session.request(method, self.base + path, headers=headers, json=body) as response:
                text = await response.text()
                try:
                    data = json.loads(text)
                except ValueError:
                    data = {"error": "non_json"}
                if not isinstance(data, dict):
                    data = {"error": "non_object_json"}
                self.http_statuses[response.status] += 1
                return response.status, data, time.monotonic() - start
        except (aiohttp.ClientError, asyncio.TimeoutError) as error:
            self.http_statuses[0] += 1
            return 0, {"error": type(error).__name__}, time.monotonic() - start

    async def user(self):
        status, body, _ = await self.request("POST", "/auth/token")
        assert status == 201, f"token issuance failed: {status}"
        return body["token"], body["user_id"]

    async def show(self, labels):
        status, body, _ = await self.request("POST", "/shows", self.admin, {
            "name": "burst-" + uuid.uuid4().hex,
            "seats": labels, "price_paise": 25000, "per_user_limit": 4,
        })
        assert status == 201, f"create show failed: {status} {body}"
        reconcile(body, labels)
        assert body["counts"]["available"] == len(labels)
        return body["id"]

    async def state(self, show):
        status, body, _ = await self.request("GET", f"/shows/{show}")
        assert status == 200, f"GET show failed: {status}"
        return body

    async def reserve(self, show, user, seats, key=None):
        result = await self.request("POST", f"/shows/{show}/reserve", user[0], {
            "seats": seats, "idempotency_key": key or uuid.uuid4().hex,
            "user_id": "spoofed-victim",
        })
        status, body, latency = result
        self.latencies.append(latency)
        self.outcomes.update(distribution([result]))
        if status in (200, 201):
            assert body["user_id"] == user[1], "identity spoofing succeeded"
            assert body["show_id"] == show and sorted(body["seats"]) == sorted(seats)
            assert body["amount_paise"] == 25000 * len(seats), "incorrect amount"
            if status == 201:
                assert body["status"] == "confirmed", "new reservation not confirmed"
                assert body["reservation_id"] not in self.created, "reservation created twice"
                self.created[body["reservation_id"]] = body
        return result

    async def stage(self, name, tasks, show, labels):
        stop = asyncio.Event()

        async def observe():
            while not stop.is_set():
                reconcile(await self.state(show), labels)
                try:
                    await asyncio.wait_for(stop.wait(), timeout=0.5)
                except asyncio.TimeoutError:
                    pass

        monitor = asyncio.create_task(observe())
        try:
            results = await asyncio.gather(*tasks, return_exceptions=True)
        finally:
            stop.set()
            await monitor
        for result in results:
            if isinstance(result, BaseException):
                raise result
        print(f"{name}: {dict(distribution(results))}", flush=True)
        assert all(result[0] in (200, 201, 409) for result in results), f"{name}: unexpected HTTP or transport failure"
        return results

    async def contract_checks(self):
        labels = ["A", "B", "C"]
        show = await self.show(labels)
        owner, other = await self.user(), await self.user()
        key = uuid.uuid4().hex
        result = await self.reserve(show, owner, ["A", "B"], key)
        assert result[0] == 201
        conflict = await self.reserve(show, owner, ["C"], key)
        assert conflict[0] == 409 and conflict[1]["error"] == "idempotency_conflict"
        partial = await self.reserve(show, other, ["A", "C"])
        assert partial[0] == 409 and partial[1]["error"] == "seat_taken"
        assert (await self.state(show))["counts"]["available"] == 1
        path = f"/reservations/{result[1]['reservation_id']}/cancel"
        assert (await self.request("POST", path, other[0]))[0] == 403
        assert (await self.request("POST", path, owner[0]))[0] == 200
        assert (await self.reserve(show, other, ["B", "A"]))[0] == 201
        assert (await self.request("POST", path, owner[0]))[0] == 200
        replay = await self.reserve(show, owner, ["B", "A"], key)
        assert replay[0] == 200 and replay[1]["status"] == "cancelled"
        state = await self.state(show)
        reconcile(state, labels)
        assert state["counts"]["confirmed"] == 2
        assert (await self.request("POST", "/shows", other[0], {}))[0] == 403
        assert (await self.request("POST", "/shows", "admin-forged", {}))[0] == 401
        assert (await self.request("GET", "/shows/not-a-uuid"))[0] == 400
        self.checks.append("ownership, spoofing, atomic multi-seat, key conflict, cancellation, rebook")


async def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("base_url")
    parser.add_argument("--seats", type=int, default=2000)
    parser.add_argument("--hot-users", type=int, default=500)
    parser.add_argument("--total", type=int, default=20000, help="spread requests in addition to other scenarios")
    parser.add_argument("--concurrency", type=int, default=500, help="maximum in-flight connections")
    parser.add_argument("--smoke", action="store_true")
    parser.add_argument("--report", help="optional JSON evidence file; contains no tokens")
    args = parser.parse_args()
    if args.smoke:
        args.seats, args.hot_users, args.total, args.concurrency = 200, 20, 100, 25
    if not (20 <= args.seats <= 10000 and args.hot_users > 0 and args.total > 0 and args.concurrency > 0):
        parser.error("seats must be 20..10000; counts and concurrency must be positive")
    admin = os.environ.get("ADMIN_TOKEN", "")
    if not admin:
        parser.error("set ADMIN_TOKEN in the environment (do not pass secrets on the command line)")
    started = time.monotonic()
    report = {"passed": False, "base_url": args.base_url, "concurrency": args.concurrency}
    connector = aiohttp.TCPConnector(limit=args.concurrency)
    timeout = aiohttp.ClientTimeout(total=240)
    async with aiohttp.ClientSession(connector=connector, timeout=timeout) as session:
        burst = Burst(session, args.base_url.rstrip("/"), admin)
        try:
            for path in ("/health/live", "/health/ready"):
                assert (await burst.request("GET", path))[0] == 200, f"{path} failed"
            labels = [f"A{index}" for index in range(args.seats)]
            show = await burst.show(labels)
            report["show_id"] = show
            print(f"Target={burst.base} show={show} concurrency={args.concurrency}", flush=True)
            users = await asyncio.gather(*(burst.user() for _ in range(max(args.hot_users, args.total))))
            hot = await burst.stage("hot-seat storm", [burst.reserve(show, user, ["A0"]) for user in users[:args.hot_users]], show, labels)
            assert sum(result[0] == 201 for result in hot) == 1, "hot seat must have one winner"
            assert sum(result[0] == 409 and result[1]["error"] == "seat_taken" for result in hot) == args.hot_users - 1
            burst.checks.append("exactly one hot-seat winner; all other attempts seat_taken")
            retry_user = await burst.user()
            key = uuid.uuid4().hex
            retry_count = 20 if args.smoke else 500
            retries = await burst.stage("same-key retries", [burst.reserve(show, retry_user, ["A1"], key) for _ in range(retry_count)], show, labels)
            assert sum(result[0] == 201 for result in retries) == 1
            assert sum(result[0] == 200 for result in retries) == retry_count - 1
            assert len({result[1]["reservation_id"] for result in retries}) == 1
            burst.checks.append("exactly one create and identical reservation on every replay")
            limited_user = await burst.user()
            limited = await burst.stage("per-user limit", [burst.reserve(show, limited_user, [label]) for label in labels[2:12]], show, labels)
            assert sum(result[0] == 201 for result in limited) == 4
            assert sum(result[0] == 409 and result[1]["error"] == "per_user_limit" for result in limited) == 6
            burst.checks.append("10 simultaneous requests produce exactly 4 bookings")
            rng = random.Random(42)
            await burst.stage("spread load", [burst.reserve(show, users[index], [rng.choice(labels)]) for index in range(args.total)], show, labels)
            final = await burst.state(show)
            reconcile(final, labels)
            confirmed = [body for body in burst.created.values() if body["show_id"] == show]
            owned_seats = [seat for body in confirmed for seat in body["seats"]]
            assert len(owned_seats) == len(set(owned_seats)), "double-sell detected"
            assert final["counts"]["confirmed"] == len(owned_seats), "committed state differs from successful responses"
            assert set(owned_seats) == {seat["label"] for seat in final["seats"] if seat["status"] == "confirmed"}
            assert max(Counter(body["user_id"] for body in confirmed).values(), default=0) <= 4
            async with session.get(burst.base + "/metrics") as response:
                burst.http_statuses[response.status] += 1
                assert response.status == 200, "metrics unavailable"
                samples = [sample for family in text_string_to_metric_families(await response.text()) for sample in family.samples]
            available = [sample.value for sample in samples if sample.name == "seats_available" and sample.labels.get("show_id") == show]
            assert available == [final["counts"]["available"]], "metrics/API divergence"
            burst.checks.append("continuous reconciliation, final unique ownership and snapshot metrics")
            await burst.contract_checks()
            report.update(passed=True, final_counts=final["counts"], checks=burst.checks)
        except (AssertionError, KeyError, TypeError, ValueError, aiohttp.ClientError, asyncio.TimeoutError) as error:
            report["failure"] = f"{type(error).__name__}: {error}"
        finally:
            latencies = sorted(burst.latencies)
            report.update(outcomes=dict(burst.outcomes), elapsed_seconds=round(time.monotonic() - started, 3))
            report["http_statuses"] = dict(burst.http_statuses)
            report["server_errors"] = sum(count for status, count in burst.http_statuses.items() if status >= 500)
            report["transport_errors"] = burst.http_statuses[0]
            if latencies:
                report["latency_ms"] = {name: round(latencies[min(len(latencies)-1, int(len(latencies)*quantile))] * 1000, 2) for name, quantile in (("p50", .50), ("p95", .95), ("p99", .99))}
            print(json.dumps(report, indent=2), flush=True)
            if args.report:
                with open(args.report, "w", encoding="utf-8") as output:
                    json.dump(report, output, indent=2)
    return 0 if report["passed"] else 1


if __name__ == "__main__":
    if not __debug__:
        sys.exit("Do not use python -O: contract assertions must remain enabled")
    sys.exit(asyncio.run(main()))