import unittest
from unittest.mock import AsyncMock

from burst import Burst, distribution, reconcile


class HarnessTests(unittest.IsolatedAsyncioTestCase):
    def test_reconciliation(self):
        show = {"seats": [{"label": "A", "status": "available"}],
                "counts": {"available": 1, "held": 0, "confirmed": 0}, "total_seats": 1}
        reconcile(show, ["A"])
        show["counts"]["available"] = 2
        with self.assertRaises(AssertionError):
            reconcile(show, ["A"])
        show["counts"]["available"] = 1
        with self.assertRaises(AssertionError):
            reconcile(show, ["A", "B"])

    def test_distribution_preserves_errors(self):
        result = distribution([(201, {}, 0), (0, {"error": "timeout"}, 0), (503, {"error": "unavailable"}, 0)])
        self.assertEqual(result, {"confirmed": 1, "0:timeout": 1, "503:unavailable": 1})

    async def test_wrong_identity_is_rejected(self):
        burst = Burst(None, "", "")
        burst.request = AsyncMock(return_value=(201, {"user_id": "victim"}, 0))
        with self.assertRaisesRegex(AssertionError, "identity"):
            await burst.reserve("show", ("token", "owner"), ["A"])

    async def test_stage_rejects_transport_failure(self):
        burst = Burst(None, "", "")
        burst.state = AsyncMock(return_value={"seats": [{"label": "A", "status": "available"}],
                            "counts": {"available": 1, "held": 0, "confirmed": 0}, "total_seats": 1})
        task = AsyncMock(return_value=(0, {"error": "timeout"}, 0))
        with self.assertRaisesRegex(AssertionError, "transport"):
            await burst.stage("test", [task()], "show", ["A"])


if __name__ == "__main__":
    unittest.main()