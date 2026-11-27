import base64
import importlib.util
import json
import os
from pathlib import Path
import tempfile
import time
import unittest

spec = importlib.util.spec_from_file_location("provider_usage", Path(__file__).with_name("provider_usage.py"))
pu = importlib.util.module_from_spec(spec)
spec.loader.exec_module(pu)


def make_jwt(claims):
    header = base64.urlsafe_b64encode(b'{"alg":"none"}').decode().rstrip("=")
    body = base64.urlsafe_b64encode(json.dumps(claims).encode()).decode().rstrip("=")
    return f"{header}.{body}.sig"


class ProviderUsageTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.home = Path(self.temp.name).resolve()
        self.old_home = os.environ.get("CAGE_TEST_HOME")
        os.environ["CAGE_TEST_HOME"] = str(self.home)
        self.addCleanup(self._restore_env)

    def _restore_env(self):
        if self.old_home is None:
            os.environ.pop("CAGE_TEST_HOME", None)
        else:
            os.environ["CAGE_TEST_HOME"] = self.old_home
        os.environ.pop("CAGE_PROVIDER_CLI_BINARY", None)

    def test_codex_plus_and_pro_rate_limits_parsing(self):
        now = 1790000000
        # Pro plan: single 7-day rolling window (10080 mins), secondary=None -> reported + single_window=seven_day
        has_exp, windows, single, plan = pu.parse_codex_rate_limits(
            {
                "limitId": "codex",
                "primary": {"usedPercent": 7, "windowDurationMins": 10080, "resetsAt": now + 86400},
                "secondary": None,
                "planType": "pro",
            },
            now,
            observed_at=now,
        )
        self.assertTrue(has_exp)
        self.assertEqual(single, "seven_day")
        self.assertEqual(plan, "pro")
        self.assertEqual(set(windows.keys()), {"seven_day"})
        self.assertEqual(windows["seven_day"]["used_percentage"], 7)

        # Plus plan: 5-hour (300 mins) + 7-day (10080 mins)
        has_exp2, windows2, single2, plan2 = pu.parse_codex_rate_limits(
            {
                "primary": {"usedPercent": 32.5, "windowDurationMins": 300, "resetsAt": now + 3600},
                "secondary": {"usedPercent": 64, "windowDurationMins": 10080, "resetsAt": now + 300000},
                "planType": "plus",
            },
            now,
            observed_at=now,
        )
        self.assertTrue(has_exp2)
        self.assertEqual(single2, "")
        self.assertEqual(plan2, "plus")
        self.assertEqual(windows2["five_hour"]["used_percentage"], 32.5)
        self.assertEqual(windows2["seven_day"]["used_percentage"], 64)

    def test_antigravity_quota_screen_parsing_and_identity_guard(self):
        now = 1790000000
        raw_tui = (
            b"\x1b[10;1H\xe2\x94\x94 Models & Quota\r\n"
            b"  Account: dev@agy.example\r\n"
            b"GEMINI MODELS\r\n"
            b"  Weekly Limit Remaining\r\n"
            b"[\xe2\x96\x88\xe2\x96\x88\xe2\x96\x91] 85.34%\r\n"
            b"Refreshes in 5d 23h 53m\r\n"
            b"  Five Hour Limit Remaining\r\n"
            b"[\xe2\x96\x88\xe2\x96\x88\xe2\x96\x91] 96.46%\r\n"
            b"Refreshes in 4h 39m\r\n"
            b"CLAUDE AND GPT MODELS\r\n"
            b"  Weekly Limit Remaining\r\n"
            b"[\xe2\x96\x88\xe2\x96\x88\xe2\x96\x88] 100.00%\r\n"
            b"Quota available\r\n"
            b"  Five Hour Limit Remaining\r\n"
            b"[\xe2\x96\x88\xe2\x96\x88\xe2\x96\x88] 100.00%\r\n"
            b"Quota available\r\n"
            b"  esc Close\r\n"
            b"auto \xc2\xb7 (Google AI Ultra)\r\n"
        )
        status, windows, groups, plan, email = pu.parse_agy_quota_screen(raw_tui, now)
        self.assertEqual(status, "reported")
        self.assertEqual(email, "dev@agy.example")
        self.assertEqual(plan, "Google AI Ultra")
        self.assertEqual(windows["seven_day"]["used_percentage"], 14.7)
        self.assertEqual(windows["seven_day"]["resets_at"], now + 5 * 86400 + 23 * 3600 + 53 * 60)
        self.assertEqual(windows["five_hour"]["used_percentage"], 3.5)
        self.assertEqual(windows["five_hour"]["resets_at"], now + 4 * 3600 + 39 * 60)
        self.assertEqual(len(groups), 2)
        self.assertEqual(groups[0]["id"], "gemini")
        self.assertEqual(groups[1]["id"], "partner")
        self.assertEqual(groups[1]["five_hour"]["used_percentage"], 0)
        self.assertEqual(groups[1]["five_hour"]["resets_at"], 0)

    def test_codex_passive_rollout_scan_and_identity_isolation(self):
        aid = "a" * 24
        base = pu.account_base("codex", aid)
        cfg = pu.config_dir_for("codex", base)
        cfg.mkdir(parents=True, exist_ok=True)
        tok = make_jwt(
            {
                "email": "pro@codex.example",
                "email_verified": True,
                "https://api.openai.com/auth": {"chatgpt_account_id": "org_1", "chatgpt_plan_type": "pro"},
            }
        )
        (cfg / "auth.json").write_text(json.dumps({"tokens": {"id_token": tok, "access_token": tok}}))
        now = int(time.time())
        rollout_dir = cfg / "sessions/2026/11/27"
        rollout_dir.mkdir(parents=True)
        event = {
            "timestamp": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(now - 30)),
            "type": "event_msg",
            "payload": {
                "type": "token_count",
                "rate_limits": {
                    "primary": {"used_percent": 12.0, "window_minutes": 10080, "resets_at": now + 86400},
                    "secondary": None,
                    "plan_type": "pro",
                },
            },
        }
        (rollout_dir / "rollout-1.jsonl").write_text(json.dumps(event) + "\n")

        snap = pu.snapshot_account("codex", aid)
        self.assertTrue(snap["logged_in"])
        self.assertEqual(snap["quota"]["seven_day"]["used_percentage"], 12)
        self.assertEqual(snap["quota"]["single_window"], "seven_day")

        # Wrong expected subject must reject immediately without exposing quota
        wrong = pu.query_usage("codex", aid, "b" * 64)
        self.assertEqual(wrong["usage_status"], "identity_changed")
        self.assertNotIn("quota", wrong)


if __name__ == "__main__":
    unittest.main()
