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
import tomllib
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
    def test_codex_existing_tui_settings_remain_valid(self):
        with tempfile.TemporaryDirectory() as directory:
            home = Path(directory)
            cfg = home / '.codex'
            cfg.mkdir()
            path = cfg / 'config.toml'
            for header in ('[tui]', '[ "tui" ] # preferences', "['tui']"):
                path.write_text(header + '\nnotifications = true\n[other]\nvalue = 1\n')
                with patch.object(Path, 'home', return_value=home):
                    helper.CodexAdapter().ensure_profile(cfg)
                    first = path.read_text()
                    helper.CodexAdapter().ensure_profile(cfg)
                self.assertEqual(path.read_text(), first)
                parsed = tomllib.loads(first)
                self.assertEqual(parsed['tui'], {'notifications': True, 'screen_reader_detection_done': True})
                self.assertEqual(parsed['other']['value'], 1)

    def test_login_url_does_not_consume_status_lines(self):
        url = 'https://auth.openai.com/oauth/authorize?client_id=test&state=expected'
        for suffix in ('\nWaiting for authentication...', '\r\n\r\nWaiting for authentication...', '\nDone'):
            self.assertEqual(helper.extract_stream_login_url('\x1b[32m' + url + '\x1b[0m' + suffix), url)

    def setUp(self):
        temp = tempfile.TemporaryDirectory()
        self.addCleanup(temp.cleanup)
        self.home = Path(temp.name)

    def test_antigravity_isolation_and_credential_snapshot(self):
        agy = helper.ADAPTERS["antigravity"]
        (self.home / ".gitconfig").write_text("[user]\nname = Test\n")
        with patch.object(Path, "home", return_value=self.home):
            base = helper.account_base("antigravity", "1" * 24)
            cfg = agy.config_dir(base)
            agy.ensure_profile(cfg)
            with patch.dict(os.environ, {"GEMINI_API_KEY": "secret-host-key", "GOOGLE_APPLICATION_CREDENTIALS": "/tmp/adc.json"}):
                env = agy.environment(base)
            self.assertNotIn("GEMINI_API_KEY", env)
            self.assertNotIn("GOOGLE_APPLICATION_CREDENTIALS", env)
            self.assertEqual(env["AGY_CLI_FORCE_FILE_TOKEN_STORAGE"], "1")
            self.assertEqual(Path(env["ANTIGRAVITY_GEMINI_DIR"]).resolve(), cfg.resolve())
            if os.name != "nt":
                isolated_home = Path(env["HOME"])
                self.assertEqual((isolated_home / ".gemini").resolve(), cfg.resolve())
                self.assertEqual((isolated_home / ".gitconfig").resolve(), (self.home / ".gitconfig").resolve())

            token_file = cfg / "antigravity-cli" / "antigravity-oauth-token"
            token_file.write_text(json.dumps({
                "token": {
                    "access_token": "secret-access-token",
                    "token_type": "Bearer",
                    "refresh_token": "secret-refresh-token",
                    "expiry": "2099-11-25T20:03:03.0686532+09:00",
                    "expires_in": 3599,
                },
                "auth_method": "consumer",
                "id_token_email": "Dev@Example.test",
            }))
            snap = agy.identity(cfg)
            self.assertTrue(snap["logged_in"])
            self.assertEqual(snap["email"], "dev@example.test")
            self.assertEqual(snap["auth_state"], "credentials_present")
            self.assertNotIn("secret-access-token", json.dumps(snap))

            agy.trust_workspace(cfg, "/c/Users/demo/project")
            settings = json.loads((cfg / "antigravity-cli" / "settings.json").read_text())
            self.assertIn("/c/Users/demo/project", settings["trustedWorkspaces"])
            self.assertTrue((cfg / "antigravity-cli" / "jetski_state.pbtxt").is_file())

    def test_archive_login_copies_oauth_token_into_managed_slot(self):
        with patch.object(Path, "home", return_value=self.home):
            default_cli = self.home / ".gemini" / "antigravity-cli"
            (default_cli / "cache").mkdir(parents=True)
            (default_cli / "antigravity-oauth-token").write_text(json.dumps({
                "token": {
                    "access_token": "secret-agy-tok",
                    "refresh_token": "refresh-agy-tok",
                    "expiry": "2099-01-01T00:00:00Z",
                },
                "auth_method": "consumer",
                "id_token_email": "oauth@example.test",
            }))
            (default_cli / "cache" / "acp-auth-state.json").write_text('{"methodId":"google-login"}')
            expected = helper.compute_subject("oauth@example.test", "")
            out = io.StringIO()
            with contextlib.redirect_stdout(out):
                rc = helper.run_provider(["antigravity", "archive-login", "d" * 24, "default", expected])
            self.assertEqual(rc, 0)
            result = json.loads(out.getvalue())
            self.assertTrue(result["logged_in"])
            self.assertEqual(result["email"], "oauth@example.test")
            copied_state = self.home / ".local/share/cage/antigravity-accounts" / ("d" * 24) / "config/antigravity-cli/cache/acp-auth-state.json"
            self.assertTrue(copied_state.is_file())

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
            self.assertNotIn("--device-auth", codex.login_command("/usr/bin/codex", [], scoped=True))
            self.assertIn("--device-auth", codex.login_command("/usr/bin/codex", ["--device-auth"], scoped=False))

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

            codex.trust_workspace(cfg, "/c/Users/demo/project")
            toml_text = (cfg / "config.toml").read_text()
            self.assertIn('[projects."/c/Users/demo/project"]', toml_text)
            self.assertIn('trust_level = "trusted"', toml_text)

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

    def test_antigravity_home_excludes_shared_credentials_and_cleans_legacy_links(self):
        agy = helper.ADAPTERS["antigravity"]
        for secret_dir in (".ssh", ".codex", ".claude", ".aws", ".gnupg", ".kube"):
            (self.home / secret_dir).mkdir(parents=True)
            (self.home / secret_dir / "secret").write_text("top-secret")
        (self.home / ".claude.json").write_text('{"oauthAccount":"x"}')
        (self.home / ".netrc").write_text("machine example.com password secret")
        (self.home / ".local/bin").mkdir(parents=True)
        (self.home / ".local/share/cage").mkdir(parents=True)
        (self.home / ".gitconfig").write_text("[user]\nname = Safe\n")

        with patch.object(Path, "home", return_value=self.home):
            base = helper.account_base("antigravity", "e" * 24)
            cfg = agy.config_dir(base)
            home_dir = agy.account_home(base)
            home_dir.mkdir(parents=True, exist_ok=True)
            if os.name != "nt":
                (home_dir / ".ssh").symlink_to(self.home / ".ssh", target_is_directory=True)
                agy.ensure_home(base, cfg)
                for blocked in (".ssh", ".codex", ".claude", ".claude.json", ".aws", ".gnupg", ".kube", ".netrc"):
                    self.assertFalse((home_dir / blocked).exists(), f"{blocked} leaked into isolated home")
                    self.assertFalse((home_dir / blocked).is_symlink(), f"{blocked} symlink remained")
                self.assertFalse((home_dir / ".local/share/cage").exists())
                self.assertTrue((home_dir / ".local/bin").is_symlink())
                self.assertTrue((home_dir / ".gitconfig").is_symlink())

    def test_exact_session_binding_and_workspace_matching(self):
        import sqlite3
        with tempfile.TemporaryDirectory() as state_tmp:
            state_root = Path(state_tmp) / "channels"
            term_root = Path(state_tmp) / "terminals"
            conn_id = "11111111-2222-4333-8444-555555555555"
            tid1 = "0123456789abcdef"
            tid2 = "fedcba9876543210"
            codex_uuid = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"

            with patch.object(helper, "CLI_CHANNEL_ROOT", state_root), patch.object(helper, "TERMINAL_SESSION_ROOT", term_root):
                with patch.dict(os.environ, {helper.NOTIFY_ENV_VAR: conn_id, helper.TERMINAL_ID_ENV_VAR: tid1}):
                    rc = helper.handle_notify(
                        "codex",
                        [json.dumps({"type": "agent-turn-complete", "thread-id": codex_uuid})],
                    )
                self.assertEqual(rc, 0)
                bound = json.loads((term_root / "codex" / f"{tid1}.json").read_text())
                self.assertEqual(bound["session_id"], codex_uuid)
                ch_state = json.loads((state_root / conn_id / "state.json").read_text())
                self.assertEqual(ch_state["cli_id"], codex_uuid)
                self.assertEqual(ch_state["turn_done"], 1)
                self.assertEqual(helper.claimed_terminal_sessions("codex", tid2), {codex_uuid})
                self.assertEqual(helper.claimed_terminal_sessions("codex", tid1), set())

                # Verify Antigravity workspace matching rejects prefix collisions (/work/a vs /work/ab) and wildcards.
                agy_cfg = Path(state_tmp) / "agy-cfg"
                (agy_cfg / "antigravity-cli").mkdir(parents=True)
                db_path = agy_cfg / "antigravity-cli" / "conversation_summaries.db"
                db = sqlite3.connect(str(db_path))
                db.execute("CREATE TABLE conversation_summaries (conversation_id TEXT, workspace_uris TEXT, killed INTEGER, last_modified_time INTEGER)")
                ws_a = Path(state_tmp) / "work_a"
                ws_ab = Path(state_tmp) / "work_ab"
                ws_a.mkdir()
                ws_ab.mkdir()
                db.execute("INSERT INTO conversation_summaries VALUES (?, ?, 0, 200)", ("conv_ab_newer", json.dumps([ws_ab.as_uri()])))
                db.execute("INSERT INTO conversation_summaries VALUES (?, ?, 0, 100)", ("conv_a_exact", json.dumps([ws_a.as_uri()])))
                db.commit()
                db.close()

                matched_a = helper.list_workspace_sessions("antigravity", agy_cfg, str(ws_a))
                self.assertEqual(matched_a, ["conv_a_exact"])
                matched_ab = helper.list_workspace_sessions("antigravity", agy_cfg, str(ws_ab))
                self.assertEqual(matched_ab, ["conv_ab_newer"])


if __name__ == "__main__":
    unittest.main()
