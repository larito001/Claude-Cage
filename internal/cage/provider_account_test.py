"""Unit tests for isolated Antigravity and Codex CLI profiles (synthetic fixtures only)."""
import base64
import contextlib
import io
import importlib.util
import json
import os
from pathlib import Path
import sys
import tempfile
import time
import unittest
from unittest.mock import patch

sys.dont_write_bytecode = True
spec = importlib.util.spec_from_file_location("provider_account", Path(__file__).with_name("provider-account.py"))
helper = importlib.util.module_from_spec(spec)
spec.loader.exec_module(helper)


def make_jwt(claims):
    header = base64.urlsafe_b64encode(b'{"alg":"none"}').decode().rstrip("=")
    body = base64.urlsafe_b64encode(json.dumps(claims).encode()).decode().rstrip("=")
    return f"{header}.{body}.sig"


class ProviderAccountTests(unittest.TestCase):
    def setUp(self):
        temp = tempfile.TemporaryDirectory()
        self.addCleanup(temp.cleanup)
        self.home = Path(temp.name)

    def test_antigravity_isolation_and_credential_snapshot(self):
        agy = helper.ADAPTERS["antigravity"]
        with patch.object(Path, "home", return_value=self.home):
            base = helper.account_base("antigravity", "1" * 24)
            cfg = agy.config_dir(base)
            agy.ensure_profile(cfg)
            with patch.dict(os.environ, {"GEMINI_API_KEY": "secret-host-key", "GOOGLE_APPLICATION_CREDENTIALS": "/tmp/adc.json"}):
                env = agy.environment(base)
            self.assertNotIn("GEMINI_API_KEY", env)
            self.assertNotIn("GOOGLE_APPLICATION_CREDENTIALS", env)
            self.assertEqual(env["AGY_CLI_FORCE_FILE_TOKEN_STORAGE"], "1")
            self.assertEqual(env["ANTIGRAVITY_GEMINI_DIR"], str(cfg))

            (cfg / "auth.json").write_text(json.dumps({
                "email": "Dev@Example.test",
                "access_token": "secret-access-token",
                "refresh_token": "secret-refresh-token",
                "expiry": time.time() + 3600,
            }))
            snap = agy.identity(cfg)
            self.assertTrue(snap["logged_in"])
            self.assertEqual(snap["email"], "dev@example.test")
            self.assertEqual(snap["auth_state"], "credentials_present")
            self.assertNotIn("secret-access-token", json.dumps(snap))

    def test_codex_isolation_file_store_and_jwt_identity(self):
        codex = helper.ADAPTERS["codex"]
        with patch.object(Path, "home", return_value=self.home):
            base = helper.account_base("codex", "2" * 24)
            cfg = codex.config_dir(base)
            codex.ensure_profile(cfg)
            self.assertIn('cli_auth_credentials_store = "file"', (cfg / "config.toml").read_text())

            with patch.dict(os.environ, {"OPENAI_API_KEY": "sk-host", "CODEX_API_KEY": "sk-codex"}):
                env = codex.environment(base)
            self.assertNotIn("OPENAI_API_KEY", env)
            self.assertNotIn("CODEX_API_KEY", env)
            self.assertEqual(env["CODEX_HOME"], str(cfg))

            jwt_tok = make_jwt({
                "email": "CodexUser@example.test",
                "email_verified": True,
                "https://api.openai.com/auth": {
                    "chatgpt_account_id": "acct-123",
                    "chatgpt_plan_type": "pro",
                },
            })
            (cfg / "auth.json").write_text(json.dumps({
                "tokens": {"id_token": jwt_tok, "access_token": "secret-oai-token"}
            }))
            snap = codex.identity(cfg)
            self.assertTrue(snap["logged_in"])
            self.assertEqual(snap["email"], "codexuser@example.test")
            self.assertEqual(snap["organization"], "acct-123")
            self.assertEqual(snap["plan"], "pro")
            self.assertNotIn("secret-oai-token", json.dumps(snap))

    def test_accounts_never_share_directories_between_providers_or_ids(self):
        with patch.object(Path, "home", return_value=self.home):
            agy_a = helper.ADAPTERS["antigravity"].config_dir(helper.account_base("antigravity", "a" * 24))
            agy_b = helper.ADAPTERS["antigravity"].config_dir(helper.account_base("antigravity", "b" * 24))
            codex_a = helper.ADAPTERS["codex"].config_dir(helper.account_base("codex", "a" * 24))
            self.assertEqual(len({agy_a, agy_b, codex_a}), 3)

    def test_archive_login_copies_verified_default_into_managed_slot(self):
        with patch.object(Path, "home", return_value=self.home):
            default_dir = self.home / ".gemini"
            default_dir.mkdir(parents=True)
            (default_dir / "auth.json").write_text(json.dumps({
                "email": "join@example.test",
                "refresh_token": "tok-1234567890",
            }))
            expected = helper.compute_subject("join@example.test", "")
            out = io.StringIO()
            with contextlib.redirect_stdout(out):
                rc = helper.run_provider(["antigravity", "archive-login", "c" * 24, "default", expected])
            self.assertEqual(rc, 0)
            result = json.loads(out.getvalue())
            self.assertTrue(result["logged_in"])
            self.assertEqual(result["email"], "join@example.test")


if __name__ == "__main__":
    unittest.main()
