#!/usr/bin/env python3
"""Isolated guest CLI account runner and local credential inspector for non-Claude providers.

Follows the exact same isolation model as cage-claude-account:
- Each managed account owns ~/.local/share/cage/<provider>-accounts/<id>/config (0700).
- Host/global API keys and external auth env vars are stripped in managed sessions.
- File-backed credential storage is enforced so no shared keyring can cross-contaminate accounts.
- OAuth logins open through SuperCage's scoped LoginBrowser (--account-id / --login-id) inside
  the account's dedicated network namespace when configured, never touching Browser Spaces or Desktops.
- Raw tokens, paths, and callback URLs are never printed to stdout/stderr.
"""
import base64
from datetime import datetime
import glob
import hashlib
import json
import math
import os
from pathlib import Path
import re
import runpy
import shutil
import signal
import sqlite3
import subprocess
import sys
import tempfile
import time
import urllib.parse
import uuid

try:
    import tomllib
except ImportError:  # Older native Python installations can still run accounts.
    tomllib = None

DEFAULT = "default"
PRIVATE_MODE = 0o700
ACCOUNT_ID_RE = re.compile(r"^[0-9a-f]{24}$")
EMAIL_RE = re.compile(r"^[^\s@\x00-\x1f]+@[^\s@\x00-\x1f]+\.[^\s@\x00-\x1f]+$")
OPEN = "/usr/local/libexec/cage-cli/bin/cage-open" if sys.platform == "darwin" else "/usr/local/bin/cage-open"
def user_home():
    home = Path.home()
    if os.name != "nt":
        try:
            resolved = home.resolve()
            if (
                resolved.name == "home"
                and len(resolved.parents) >= 6
                and resolved.parents[1].name == "antigravity-accounts"
                and resolved.parents[2].name == "cage"
                and resolved.parents[4].name == ".local"
            ):
                return resolved.parents[5]
        except OSError:
            pass
    return home


CLI_CHANNEL_ROOT = Path(os.environ.get("CAGE_CLI_STATE_ROOT") or (user_home() / ".local/share/cage/cli-channels"))
TERMINAL_SESSION_ROOT = Path(os.environ.get("CAGE_TERMINAL_SESSION_ROOT") or (user_home() / ".local/share/cage/terminal-sessions"))
NOTIFY_ENV_VAR = "__WORKBENCH_NOTIFY_CHANNEL"
TERMINAL_ID_ENV_VAR = "__WORKBENCH_TERMINAL_ID"
TERMINAL_ID_RE = re.compile(r"^[0-9a-f]{16}$")
CODEX_SESSION_RE = re.compile(r"^[0-9a-fA-F-]{36}$")
AGY_SESSION_RE = re.compile(r"^[0-9a-zA-Z_-]{8,128}$")

_controller_context = {
    key: os.environ.pop(key)
    for key in (
        "CAGE_LOGIN_OWNER",
        "CAGE_ACCOUNT_BROWSER",
        "CAGE_PROVIDER_SUBJECT",
        "CAGE_PROVIDER_ACCOUNT_ID",
        "CAGE_NOTIFY_CONNECTION",
        "CAGE_TERMINAL_ID",
    )
    if key in os.environ
}

_runtime_path = Path("/usr/local/bin/client_runtime.py")
if _runtime_path.is_file():
    _runtime = runpy.run_path(str(_runtime_path))
else:
    def _fallback_launcher(target, executable=False, arguments=(), **_kwargs):
        if not arguments:
            return str(target)
        fd, wrapper = tempfile.mkstemp(prefix="cage-open-", suffix=".sh")
        os.close(fd)
        quoted = " ".join("'" + str(x).replace("'", "'\\''") + "'" for x in [str(target), *arguments])
        Path(wrapper).write_text(f"#!/bin/sh\nexec {quoted} \"$@\"\n", encoding="utf-8")
        os.chmod(wrapper, 0o700 if executable else 0o600)
        return wrapper

    _runtime = {
        "path": lambda value: str(value),
        "environment": lambda env: {k: v for k, v in env.items() if not k.upper().startswith("CAGE_")},
        "launcher": _fallback_launcher,
    }


def compute_subject(email, organization=""):
    clean_email = (email or "").strip().lower()
    clean_org = (organization or "").strip()
    if not clean_email:
        return ""
    return hashlib.sha256(f"{clean_email}\n{clean_org}".encode("utf-8")).hexdigest()


def decode_jwt(token):
    if not isinstance(token, str) or len(token) > 65536 or token.count(".") != 2:
        return {}
    try:
        payload = token.split(".")[1]
        payload += "=" * (-len(payload) % 4)
        claims = json.loads(base64.urlsafe_b64decode(payload))
        return claims if isinstance(claims, dict) else {}
    except (ValueError, TypeError, UnicodeError):
        return {}


def parse_expiry(value):
    if isinstance(value, bool):
        return None
    try:
        number = float(value)
        if not math.isfinite(number):
            return None
        return number / 1000 if number > 1e11 else number
    except (ValueError, TypeError):
        try:
            return datetime.fromisoformat(str(value).replace("Z", "+00:00")).timestamp()
        except (ValueError, TypeError, AttributeError):
            return None


def read_regular_json(candidates):
    for candidate in candidates:
        if not candidate.exists():
            continue
        try:
            if candidate.is_symlink() or candidate.parent.is_symlink() or not candidate.is_file():
                return None, None
            with candidate.open("rb") as stream:
                raw = stream.read((1 << 20) + 1)
            if len(raw) > (1 << 20):
                return None, None
            data = json.loads(raw)
        except (OSError, ValueError):
            return None, None
        return (candidate, data) if isinstance(data, dict) else (None, None)
    return None, None


def atomic_copy_private(src, dst):
    dst.parent.mkdir(mode=PRIVATE_MODE, parents=True, exist_ok=True)
    with tempfile.NamedTemporaryFile(dir=dst.parent, delete=False) as tmp:
        with src.open("rb") as inp:
            shutil.copyfileobj(inp, tmp)
        tmp.flush()
        os.fsync(tmp.fileno())
        tmp_name = tmp.name
    if os.name != "nt":
        os.chmod(tmp_name, 0o600)
    os.replace(tmp_name, dst)


def validate_account_id(account_id):
    if account_id != DEFAULT and not ACCOUNT_ID_RE.fullmatch(account_id or ""):
        raise ValueError("invalid account identifier")
    return account_id


def account_base(provider_id, account_id):
    validate_account_id(account_id)
    root = user_home() / ".local/share/cage" / f"{provider_id}-accounts"
    root.mkdir(mode=PRIVATE_MODE, parents=True, exist_ok=True)
    if account_id == DEFAULT:
        return root / DEFAULT
    base = root / account_id
    base.mkdir(mode=PRIVATE_MODE, parents=True, exist_ok=True)
    return base


def atomic_write_json(path, value):
    path.parent.mkdir(mode=PRIVATE_MODE, parents=True, exist_ok=True)
    with tempfile.NamedTemporaryFile(dir=path.parent, delete=False, mode="w", encoding="utf-8") as tmp:
        json.dump(value, tmp, ensure_ascii=False)
        tmp.write("\n")
        tmp.flush()
        os.fsync(tmp.fileno())
        tmp_name = tmp.name
    if os.name != "nt":
        os.chmod(tmp_name, 0o600)
    os.replace(tmp_name, path)


def valid_notify_connection(value):
    if not isinstance(value, str) or not value:
        return ""
    try:
        return str(uuid.UUID(value.strip()))
    except (ValueError, AttributeError):
        return ""


def valid_terminal_id(value):
    if not isinstance(value, str):
        return ""
    cleaned = value.strip().lower()
    return cleaned if TERMINAL_ID_RE.fullmatch(cleaned) else ""


def valid_provider_session_id(provider_id, value):
    if not isinstance(value, str):
        return ""
    cleaned = value.strip()
    if provider_id == "codex":
        return cleaned if CODEX_SESSION_RE.fullmatch(cleaned) else ""
    if provider_id == "antigravity":
        return cleaned if AGY_SESSION_RE.fullmatch(cleaned) else ""
    return ""


def _update_channel_state(conn_id, updater):
    conn = valid_notify_connection(conn_id)
    if not conn:
        return
    folder = CLI_CHANNEL_ROOT / conn
    folder.mkdir(mode=PRIVATE_MODE, parents=True, exist_ok=True)
    state_path = folder / "state.json"

    def _apply():
        try:
            state = json.loads(state_path.read_text(encoding="utf-8"))
            if not isinstance(state, dict):
                state = {}
        except (OSError, ValueError):
            state = {}
        if updater(state):
            atomic_write_json(state_path, state)

    if os.name == "nt":
        _apply()
        return
    import fcntl
    with (folder / "lock").open("a") as lock:
        fcntl.flock(lock, fcntl.LOCK_EX)
        _apply()


def record_terminal_session(provider_id, terminal_id, conn_id, session_id):
    clean_sid = valid_provider_session_id(provider_id, session_id)
    if not clean_sid:
        return False
    clean_tid = valid_terminal_id(terminal_id)
    if clean_tid:
        target = TERMINAL_SESSION_ROOT / provider_id / f"{clean_tid}.json"
        try:
            atomic_write_json(target, {"session_id": clean_sid, "updated_at": time.time()})
        except OSError:
            pass
    if valid_notify_connection(conn_id):
        def _set_cli(state):
            if state.get("cli_id") == clean_sid:
                return False
            state["cli_id"] = clean_sid
            state["updated_at"] = time.time()
            return True

        try:
            _update_channel_state(conn_id, _set_cli)
        except OSError:
            pass
    return True


def record_channel_event(conn_id, event_kind, cli_id=""):
    if event_kind not in ("turn_completed", "approval_requested"):
        return

    def _mutate(state):
        state["updated_at"] = time.time()
        if cli_id:
            state["cli_id"] = cli_id
        if event_kind == "turn_completed":
            prev = state.get("turn_done")
            state["turn_done"] = (prev if isinstance(prev, int) and prev > 0 else 0) + 1
        elif event_kind == "approval_requested":
            approvals = state.get("approvals")
            if not isinstance(approvals, list):
                approvals = []
            approvals.append({"id": uuid.uuid4().hex[:16]})
            state["approvals"] = approvals[-32:]
        return True

    _update_channel_state(conn_id, _mutate)


def _match_agy_workspace(raw_ws, cwd):
    if not raw_ws or not cwd:
        return False
    clean_cwd = str(cwd).rstrip("/") or "/"
    try:
        uri = Path(clean_cwd).as_uri().rstrip("/")
    except ValueError:
        uri = "file://" + clean_cwd
    targets = {clean_cwd, uri, "file://" + clean_cwd}
    text = str(raw_ws).strip()
    items = None
    try:
        parsed = json.loads(text)
        if isinstance(parsed, list):
            items = [str(x) for x in parsed if isinstance(x, str)]
        elif isinstance(parsed, str):
            items = [parsed]
    except ValueError:
        pass
    if items is None:
        items = [p.strip("\"'[]") for p in re.split(r"[\s,;]+", text) if p]
    for item in items:
        norm = item.rstrip("/") or "/"
        unquoted = urllib.parse.unquote(norm).rstrip("/") or "/"
        if norm in targets or unquoted in targets:
            return True
    return False


def list_workspace_sessions(provider_id, config_dir, cwd):
    results = []
    if provider_id == "codex":
        for db in sorted(glob.glob(str(Path(config_dir) / "state_*.sqlite")), reverse=True):
            try:
                conn = sqlite3.connect(f"file:{db}?mode=ro", uri=True)
                rows = conn.execute(
                    "SELECT id FROM threads WHERE cwd=? AND COALESCE(archived,0)=0 ORDER BY updated_at DESC",
                    (cwd,),
                ).fetchall()
                conn.close()
                for (rid,) in rows:
                    sid = valid_provider_session_id("codex", str(rid or ""))
                    if sid and sid not in results:
                        results.append(sid)
            except (sqlite3.Error, OSError):
                pass
    elif provider_id == "antigravity":
        db_path = Path(config_dir) / "antigravity-cli" / "conversation_summaries.db"
        if db_path.is_file():
            try:
                conn = sqlite3.connect(f"file:{db_path}?mode=ro", uri=True)
                rows = conn.execute(
                    "SELECT conversation_id, workspace_uris FROM conversation_summaries WHERE COALESCE(killed,0)=0 ORDER BY last_modified_time DESC"
                ).fetchall()
                conn.close()
                for cid, ws in rows:
                    sid = valid_provider_session_id("antigravity", str(cid or ""))
                    if sid and sid not in results and _match_agy_workspace(ws, cwd):
                        results.append(sid)
            except (sqlite3.Error, OSError):
                pass
    return results


def claimed_terminal_sessions(provider_id, exclude_tid=""):
    claimed = set()
    folder = TERMINAL_SESSION_ROOT / provider_id
    if not folder.is_dir():
        return claimed
    for entry in folder.glob("*.json"):
        if entry.stem == exclude_tid:
            continue
        try:
            sid = valid_provider_session_id(provider_id, json.loads(entry.read_text(encoding="utf-8")).get("session_id", ""))
            if sid:
                claimed.add(sid)
        except (OSError, ValueError):
            pass
    return claimed


def extract_explicit_resume_id(provider_id, extra_args):
    args = [str(x) for x in (extra_args or ())]
    if provider_id == "codex":
        for idx, arg in enumerate(args):
            if arg == "resume" and idx + 1 < len(args):
                return valid_provider_session_id("codex", args[idx + 1])
    elif provider_id == "antigravity":
        for idx, arg in enumerate(args):
            if arg == "--resume" and idx + 1 < len(args):
                return valid_provider_session_id("antigravity", args[idx + 1])
            if arg.startswith("--resume="):
                return valid_provider_session_id("antigravity", arg.split("=", 1)[1])
    return ""


def start_session_watcher(provider_id, config_dir, cwd, terminal_id, conn_id, target_pid):
    clean_tid = valid_terminal_id(terminal_id)
    clean_conn = valid_notify_connection(conn_id)
    if os.name == "nt" or (not clean_tid and not clean_conn):
        return
    baseline = list_workspace_sessions(provider_id, config_dir, cwd)
    script = (
        "import json,os,runpy,sys,time\n"
        "m=runpy.run_path(sys.argv[1])\n"
        "prov,cdir,cwd,tid,conn,pid,base=sys.argv[2],sys.argv[3],sys.argv[4],sys.argv[5],sys.argv[6],int(sys.argv[7]),set(json.loads(sys.argv[8]))\n"
        "tfile=m['TERMINAL_SESSION_ROOT']/prov/f'{tid}.json' if tid else None\n"
        "end=time.monotonic()+600\n"
        "while time.monotonic()<end:\n"
        " time.sleep(1.5)\n"
        " if tfile and tfile.is_file():break\n"
        " try:os.kill(pid,0)\n"
        " except OSError:break\n"
        " claimed=m['claimed_terminal_sessions'](prov,tid)\n"
        " for sid in m['list_workspace_sessions'](prov,cdir,cwd):\n"
        "  if sid not in base and sid not in claimed:\n"
        "   m['record_terminal_session'](prov,tid,conn,sid)\n"
        "   sys.exit(0)\n"
    )
    try:
        subprocess.Popen(
            [
                sys.executable,
                "-I",
                "-c",
                script,
                str(Path(__file__).resolve()),
                provider_id,
                str(config_dir),
                str(cwd),
                clean_tid,
                clean_conn,
                str(target_pid),
                json.dumps(baseline),
            ],
            stdin=subprocess.DEVNULL,
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
            start_new_session=True,
            close_fds=True,
        )
    except OSError:
        pass


class AntigravityAdapter:
    id = "antigravity"
    name = "Antigravity"
    binary_name = "agy"
    shim_dir = "/usr/local/lib/cage-antigravity-bin"
    external_env_keys = (
        "GEMINI_API_KEY",
        "GOOGLE_API_KEY",
        "GOOGLE_GEMINI_BASE_URL",
        "GOOGLE_APPLICATION_CREDENTIALS",
        "GOOGLE_CLOUD_PROJECT",
        "GOOGLE_CLOUD_LOCATION",
        "GOOGLE_CLOUD_QUOTA_PROJECT",
        "GOOGLE_GENAI_USE_VERTEXAI",
        "AGY_ADC_AUTH",
    )

    def config_dir(self, base):
        return user_home() / ".gemini" if base.name == DEFAULT else base / "config"

    def account_home(self, base):
        return user_home() if base.name == DEFAULT else base / "home"

    excluded_home_entries = frozenset({
        ".gemini",
        ".claude",
        ".claude.json",
        ".codex",
        ".ssh",
        ".gnupg",
        ".aws",
        ".azure",
        ".kube",
        ".docker",
        ".netrc",
        ".npmrc",
        ".pypirc",
        ".git-credentials",
        ".pki",
        ".password-store",
        ".config",
        ".local",
        ".cache",
    })

    def ensure_home(self, base, config_dir):
        if base.name == DEFAULT or os.name == "nt":
            return
        real = user_home()
        home_dir = self.account_home(base)
        home_dir.mkdir(mode=PRIVATE_MODE, parents=True, exist_ok=True)
        config_dir.mkdir(mode=PRIVATE_MODE, parents=True, exist_ok=True)
        gemini_link = home_dir / ".gemini"
        if gemini_link.is_symlink():
            try:
                same_target = gemini_link.resolve() == config_dir.resolve()
            except OSError:
                same_target = False
            if not same_target:
                gemini_link.unlink()
                gemini_link.symlink_to(config_dir, target_is_directory=True)
        elif not gemini_link.exists():
            gemini_link.symlink_to(config_dir, target_is_directory=True)

        # Clean up any legacy symlinks that exposed shared credentials/configs.
        for excluded in self.excluded_home_entries:
            if excluded == ".gemini":
                continue
            stale = home_dir / excluded
            if stale.is_symlink():
                try:
                    stale.unlink()
                except OSError:
                    pass

        for rel_dir in (Path(".config"), Path(".cache"), Path(".local/share")):
            try:
                (home_dir / rel_dir).mkdir(mode=PRIVATE_MODE, parents=True, exist_ok=True)
            except OSError:
                pass
        real_local_bin = real / ".local/bin"
        local_bin_link = home_dir / ".local/bin"
        if real_local_bin.exists() and not local_bin_link.exists() and not local_bin_link.is_symlink():
            try:
                local_bin_link.symlink_to(real_local_bin, target_is_directory=True)
            except OSError:
                pass

        try:
            entries = list(real.iterdir())
        except OSError:
            entries = []
        for item in entries:
            if item.name in self.excluded_home_entries:
                continue
            link = home_dir / item.name
            if link.exists() or link.is_symlink():
                continue
            try:
                link.symlink_to(item, target_is_directory=item.is_dir())
            except OSError:
                pass

    def ensure_hooks(self, config_dir):
        hooks_dir = config_dir / "config"
        hooks_dir.mkdir(mode=PRIVATE_MODE, parents=True, exist_ok=True)
        hooks_file = hooks_dir / "hooks.json"
        if hooks_file.is_symlink():
            return
        data = {}
        try:
            if hooks_file.is_file():
                parsed = json.loads(hooks_file.read_text(encoding="utf-8"))
                if isinstance(parsed, dict):
                    data = parsed
        except (OSError, ValueError):
            data = {}
        desired = {
            "Stop": [
                {
                    "type": "command",
                    "command": "/usr/local/bin/cage-provider-account antigravity notify",
                    "timeout": 5,
                }
            ]
        }
        if data.get("cage-notify") != desired:
            data["cage-notify"] = desired
            atomic_write_json(hooks_file, data)

    def ensure_profile(self, config_dir):
        cli_dir = config_dir / "antigravity-cli"
        cache_dir = cli_dir / "cache"
        cache_dir.mkdir(mode=PRIVATE_MODE, parents=True, exist_ok=True)
        (config_dir / "config").mkdir(mode=PRIVATE_MODE, parents=True, exist_ok=True)
        settings_file = cli_dir / "settings.json"
        if not settings_file.exists():
            atomic_write_json(
                settings_file,
                {
                    "toolPermission": "always-proceed",
                    "artifactReviewPolicy": "always-proceed",
                    "allowNonWorkspaceAccess": True,
                },
            )
        onboarding_file = cache_dir / "onboarding.json"
        if not onboarding_file.exists():
            atomic_write_json(
                onboarding_file,
                {
                    "consumerOnboardingComplete": True,
                    "enterpriseOnboardingComplete": True,
                    "onboardingComplete": True,
                },
            )
        self.ensure_hooks(config_dir)
        self.sync_managed_mcp(config_dir)

    def sync_managed_mcp(self, config_dir):
        default_mcp = user_home() / ".gemini/config/mcp_config.json"
        target_mcp = config_dir / "config/mcp_config.json"
        if default_mcp.resolve() == target_mcp.resolve() or target_mcp.is_symlink():
            return
        relay_prog = str(user_home() / ".local/share/mcp-relay/relay.py")
        relay_eps = {
            str(user_home() / ".config/mcp-relay/computer.json"),
            str(user_home() / ".config/mcp-relay/computer-antigravity.json"),
        }
        is_managed_relay = (
            lambda entry: isinstance(entry, dict)
            and isinstance(entry.get("args"), list)
            and len(entry["args"]) >= 2
            and entry["args"][0] == relay_prog
            and entry["args"][1] in relay_eps
        )
        shared_entry = None
        try:
            if default_mcp.is_file() and not default_mcp.is_symlink():
                src = json.loads(default_mcp.read_text(encoding="utf-8"))
                cand = (src.get("mcpServers") or {}).get("computer") if isinstance(src, dict) else None
                if is_managed_relay(cand):
                    shared_entry = cand
        except (OSError, ValueError):
            shared_entry = None
        try:
            raw_dst = target_mcp.read_text(encoding="utf-8") if target_mcp.is_file() else ""
            dst = json.loads(raw_dst) if raw_dst.strip() else {}
            if not isinstance(dst, dict):
                return
        except (OSError, ValueError):
            return
        servers = dst.get("mcpServers")
        if not isinstance(servers, dict):
            servers = {}
        current = servers.get("computer")
        is_ours = is_managed_relay(current)
        if shared_entry is not None:
            if current is None or is_ours:
                if current != shared_entry:
                    servers["computer"] = shared_entry
                    dst["mcpServers"] = servers
                    atomic_write_json(target_mcp, dst)
        elif is_ours:
            del servers["computer"]
            if servers:
                dst["mcpServers"] = servers
            else:
                dst.pop("mcpServers", None)
            atomic_write_json(target_mcp, dst)

    def environment(self, base):
        env = os.environ.copy()
        if os.name != "nt":
            for ssh_key in ("SSH_CLIENT", "SSH_CONNECTION", "SSH_TTY"):
                env.pop(ssh_key, None)
        real = user_home()
        parts = [p for p in env.get("PATH", os.defpath).split(os.pathsep) if p and p != self.shim_dir]
        env["PATH"] = os.pathsep.join([str(real / ".local/bin"), *parts])
        if base.name == DEFAULT:
            if os.name != "nt":
                env["HOME"] = str(real)
            env.pop("ANTIGRAVITY_GEMINI_DIR", None)
            return _runtime["environment"](env)
        config_dir = self.config_dir(base)
        self.ensure_home(base, config_dir)
        if os.name != "nt":
            env["HOME"] = str(self.account_home(base))
        env["ANTIGRAVITY_GEMINI_DIR"] = str(config_dir)
        env["AGY_CLI_FORCE_FILE_TOKEN_STORAGE"] = "1"
        for key in self.external_env_keys:
            env.pop(key, None)
        return _runtime["environment"](env)

    def auth_candidates(self, config_dir):
        return [
            config_dir / "antigravity-cli" / "antigravity-oauth-token",
            config_dir / "antigravity-oauth-token",
            config_dir / "auth.json",
            config_dir / "antigravity-cli" / "auth.json",
            config_dir / "config" / "auth.json",
        ]

    def identity(self, config_dir, _env=None):
        _, data = read_regular_json(self.auth_candidates(config_dir))
        absent = {"logged_in": False, "auth_state": "signed_out", "usage_status": "unavailable", "subject": ""}
        if data is None:
            return absent
        candidates = [data]
        for key in ("account", "user", "token", "tokens", "credentials", "oauthToken"):
            if isinstance(data.get(key), dict):
                candidates.append(data[key])

        def field(*names):
            return next(
                (c[n].strip() for c in candidates for n in names if isinstance(c.get(n), str) and c[n].strip()),
                "",
            )

        access = field("access_token", "accessToken")
        refresh = field("refresh_token", "refreshToken")
        if not access and not refresh:
            return absent
        expiry = next(
            (
                parse_expiry(c[n])
                for c in candidates
                for n in ("expiry", "expires_at", "expiresAt", "expiry_date", "access_token_expires_at")
                if n in c
            ),
            None,
        )
        expired = expiry is not None and expiry <= time.time()
        if expired and not refresh:
            return {**absent, "auth_state": "expired"}
        email = field("email", "emailAddress", "user_email", "id_token_email").lower()
        if not email:
            claims = decode_jwt(field("id_token", "idToken"))
            if isinstance(claims.get("email"), str) and claims.get("email_verified") is not False:
                email = claims["email"].strip().lower()
        if not EMAIL_RE.fullmatch(email) or len(email) > 254:
            return {**absent, "auth_state": "identity_missing"}
        subject = compute_subject(email, "")
        auth_method = field("auth_method", "authMethod").lower()
        plan = "enterprise" if auth_method == "enterprise" else "antigravity"
        return {
            "logged_in": True,
            "email": email,
            "organization": "",
            "plan": plan,
            "usage_status": "unavailable",
            "auth_state": "refresh_required" if expired or not access else "credentials_present",
            "subject": subject,
        }

    def copy_auth(self, source_dir, target_dir):
        selected, _ = read_regular_json(self.auth_candidates(source_dir))
        if selected is None:
            return False
        rel = selected.relative_to(source_dir)
        atomic_copy_private(selected, target_dir / rel)
        for extra_rel in (
            Path("antigravity-cli/cache/onboarding.json"),
            Path("antigravity-cli/cache/acp-auth-state.json"),
        ):
            extra = source_dir / extra_rel
            if extra.is_file() and not extra.is_symlink() and not extra.parent.is_symlink():
                try:
                    atomic_copy_private(extra, target_dir / extra_rel)
                except OSError:
                    pass
        return True

    def desktop_emails(self):
        home = user_home()
        directories = [home / ".config/Antigravity"]
        profiles = home / ".local/share/desktop-profiles"
        if profiles.is_dir():
            for p in profiles.iterdir():
                if not ACCOUNT_ID_RE.fullmatch(p.name) or p.is_symlink():
                    continue
                try:
                    prov = (p / "provider").read_text(encoding="utf-8").strip()
                except OSError:
                    prov = ""
                if prov == "antigravity":
                    directories.append(p / "data")
        emails = set()
        for directory in directories:
            path = directory / "app_storage.json"
            try:
                if directory.is_symlink() or path.is_symlink() or not path.is_file():
                    continue
                with path.open("rb") as stream:
                    raw = stream.read((1 << 20) + 1)
                if len(raw) > (1 << 20):
                    continue
                value = json.loads(raw).get("agy.onboarding.lastLoginUsername")
                if isinstance(value, str) and len(value) <= 254 and EMAIL_RE.fullmatch(value.strip()):
                    emails.add(value.strip().lower())
            except (ValueError, OSError, AttributeError):
                continue
        return sorted(emails)[:128]

    def login_command(self, binary, _extra_args, scoped=False):
        return [_runtime["path"](binary), "acp", "--login"]

    def run_command(self, binary, extra_args, _is_default):
        return [_runtime["path"](binary), *extra_args]

    def trust_workspace(self, config_dir, workspace):
        if not workspace:
            return
        clean_ws = str(workspace).strip()
        if not clean_ws or clean_ws == "/" or Path(clean_ws) == user_home():
            return
        cli_dir = config_dir / "antigravity-cli"
        cli_dir.mkdir(mode=PRIVATE_MODE, parents=True, exist_ok=True)
        settings_file = cli_dir / "settings.json"
        if not settings_file.is_symlink():
            data = {}
            try:
                if settings_file.is_file():
                    parsed = json.loads(settings_file.read_text(encoding="utf-8"))
                    if isinstance(parsed, dict):
                        data = parsed
            except (OSError, ValueError):
                data = {}
            trusted = data.get("trustedWorkspaces")
            if not isinstance(trusted, list):
                trusted = []
            changed = False
            for k, v in (
                ("toolPermission", "always-proceed"),
                ("artifactReviewPolicy", "always-proceed"),
                ("allowNonWorkspaceAccess", True),
            ):
                if k not in data:
                    data[k] = v
                    changed = True
            if clean_ws not in trusted:
                trusted.append(clean_ws)
                data["trustedWorkspaces"] = trusted
                changed = True
            if changed:
                atomic_write_json(settings_file, data)
        pbtxt = cli_dir / "jetski_state.pbtxt"
        if not pbtxt.exists() and not pbtxt.is_symlink():
            pbtxt_body = (
                "post_onboarding: {\n"
                "  completed_steps: POST_ONBOARDING_STEP_TYPE_MANAGER_WELCOME\n"
                "  completed_steps: POST_ONBOARDING_STEP_TYPE_USAGE_MODE\n"
                "  completed_steps: POST_ONBOARDING_STEP_TYPE_AGENT_CONFIGURATION\n"
                "  completed_steps: POST_ONBOARDING_STEP_TYPE_ADD_WORKSPACE\n"
                "}\n"
            )
            with tempfile.NamedTemporaryFile(dir=cli_dir, delete=False, mode="w", encoding="utf-8") as tmp:
                tmp.write(pbtxt_body)
                tmp.flush()
                os.fsync(tmp.fileno())
                tmp_name = tmp.name
            if os.name != "nt":
                os.chmod(tmp_name, 0o600)
            os.replace(tmp_name, pbtxt)


class CodexAdapter:
    id = "codex"
    name = "Codex"
    binary_name = "codex"
    shim_dir = "/usr/local/lib/cage-codex-bin"
    external_env_keys = (
        "OPENAI_API_KEY",
        "OPENAI_BASE_URL",
        "CODEX_API_KEY",
        "CODEX_ACCESS_TOKEN",
    )

    def config_dir(self, base):
        return user_home() / ".codex" if base.name == DEFAULT else base / "config"

    def ensure_profile(self, config_dir):
        config_dir.mkdir(mode=PRIVATE_MODE, parents=True, exist_ok=True)
        toml_file = config_dir / "config.toml"
        key_line = 'cli_auth_credentials_store = "file"'
        try:
            existing = toml_file.read_text(encoding="utf-8") if toml_file.is_file() else ""
        except OSError:
            existing = ""
        updated = existing
        if "cli_auth_credentials_store" not in updated:
            updated = key_line + "\n" + updated
        # Add the default within an existing table without duplicating it.
        # Validate the candidate so inline tables or unusual user TOML remain intact.
        if tomllib is not None:
            try:
                parsed = tomllib.loads(updated)
                if "screen_reader_detection_done" not in parsed.get("tui", {}):
                    header = re.search(r'''(?m)^[ \t]*\[[ \t]*(?:tui|"tui"|'tui')[ \t]*\][ \t]*(?:\#[^\n]*)?(?:\n|$)''', updated)
                    setting = "screen_reader_detection_done = true\n"
                    if header:
                        candidate = updated[:header.end()].rstrip("\n") + "\n" + setting + updated[header.end():]
                    else:
                        candidate = updated.rstrip("\n") + "\n\n[tui]\n" + setting
                    if tomllib.loads(candidate).get("tui", {}).get("screen_reader_detection_done") is True:
                        updated = candidate
            except (tomllib.TOMLDecodeError, TypeError):
                pass
        default_toml = user_home() / ".codex/config.toml"
        if default_toml.resolve() != toml_file.resolve():
            start_marker = "# cage-managed-mcp-start:computer\n"
            end_marker = "# cage-managed-mcp-end:computer\n"
            shared_block = ""
            try:
                if default_toml.is_file() and not default_toml.is_symlink():
                    src_toml = default_toml.read_text(encoding="utf-8")
                    if start_marker in src_toml and end_marker in src_toml:
                        _, mid = src_toml.split(start_marker, 1)
                        body, _ = mid.split(end_marker, 1)
                        shared_block = start_marker + body + end_marker
            except OSError:
                shared_block = ""
            if shared_block:
                if start_marker in updated and end_marker in updated:
                    before, rest = updated.split(start_marker, 1)
                    _, after = rest.split(end_marker, 1)
                    updated = before + shared_block + after
                elif "[mcp_servers.computer]" not in updated:
                    sep = "" if not updated or updated.endswith("\n\n") else ("\n" if updated.endswith("\n") else "\n\n")
                    updated = updated + sep + shared_block
            elif start_marker in updated and end_marker in updated:
                before, rest = updated.split(start_marker, 1)
                _, after = rest.split(end_marker, 1)
                updated = (before.rstrip("\n") + ("\n\n" if before.strip() and after.strip() else "\n" if (before.strip() or after.strip()) else "") + after.lstrip("\n"))
        if updated != existing:
            with tempfile.NamedTemporaryFile(dir=config_dir, delete=False, mode="w", encoding="utf-8") as tmp:
                tmp.write(updated)
                tmp.flush()
                os.fsync(tmp.fileno())
                tmp_name = tmp.name
            if os.name != "nt":
                os.chmod(tmp_name, 0o600)
            os.replace(tmp_name, toml_file)

    def trust_workspace(self, config_dir, workspace):
        if not workspace:
            return
        clean_ws = str(workspace).strip()
        if not clean_ws or clean_ws == "/" or "\n" in clean_ws or "\r" in clean_ws or Path(clean_ws) == user_home():
            return
        config_dir.mkdir(mode=PRIVATE_MODE, parents=True, exist_ok=True)
        toml_file = config_dir / "config.toml"
        if toml_file.is_symlink():
            return
        try:
            existing = toml_file.read_text(encoding="utf-8") if toml_file.is_file() else ""
        except OSError:
            existing = ""
        escaped = clean_ws.replace("\\", "\\\\").replace('"', '\\"')
        header = f'[projects."{escaped}"]'
        if header in existing:
            return
        suffix = "" if not existing or existing.endswith("\n") else "\n"
        updated = f"{existing}{suffix}\n{header}\ntrust_level = \"trusted\"\n"
        with tempfile.NamedTemporaryFile(dir=config_dir, delete=False, mode="w", encoding="utf-8") as tmp:
            tmp.write(updated)
            tmp.flush()
            os.fsync(tmp.fileno())
            tmp_name = tmp.name
        if os.name != "nt":
            os.chmod(tmp_name, 0o600)
        os.replace(tmp_name, toml_file)

    def environment(self, base):
        env = os.environ.copy()
        if os.name != "nt":
            for ssh_key in ("SSH_CLIENT", "SSH_CONNECTION", "SSH_TTY"):
                env.pop(ssh_key, None)
        parts = [p for p in env.get("PATH", os.defpath).split(os.pathsep) if p and p != self.shim_dir]
        env["PATH"] = os.pathsep.join([str(Path.home() / ".local/bin"), *parts])
        if base.name == DEFAULT:
            env.pop("CODEX_HOME", None)
            return _runtime["environment"](env)
        env["CODEX_HOME"] = str(self.config_dir(base))
        for key in self.external_env_keys:
            env.pop(key, None)
        return _runtime["environment"](env)

    def auth_candidates(self, config_dir):
        return [config_dir / "auth.json"]

    def identity(self, config_dir, _env=None):
        _, data = read_regular_json(self.auth_candidates(config_dir))
        absent = {"logged_in": False, "auth_state": "signed_out", "usage_status": "unavailable", "subject": ""}
        if data is None:
            return absent
        tokens = data.get("tokens") if isinstance(data.get("tokens"), dict) else data
        access = tokens.get("access_token") or tokens.get("accessToken") or ""
        refresh = tokens.get("refresh_token") or tokens.get("refreshToken") or ""
        id_tok = tokens.get("id_token") or tokens.get("idToken") or ""
        api_key = data.get("OPENAI_API_KEY") or ""
        if not any(isinstance(v, str) and v.strip() for v in (access, refresh, id_tok, api_key)):
            return absent
        claims = decode_jwt(id_tok) or decode_jwt(access)
        email = ""
        if isinstance(claims.get("email"), str) and claims.get("email_verified") is not False:
            email = claims["email"].strip().lower()
        if not email and isinstance(data.get("email"), str):
            email = data["email"].strip().lower()
        auth_claim = claims.get("https://api.openai.com/auth")
        org = ""
        plan = "codex"
        if isinstance(auth_claim, dict):
            org = str(auth_claim.get("chatgpt_account_id") or auth_claim.get("organization_id") or "").strip()
            raw_plan = auth_claim.get("chatgpt_plan_type")
            if isinstance(raw_plan, str) and raw_plan.strip():
                plan = raw_plan.strip()
        if not EMAIL_RE.fullmatch(email) or len(email) > 254:
            if isinstance(api_key, str) and len(api_key.strip()) >= 8:
                digest = hashlib.sha256(api_key.strip().encode("utf-8")).hexdigest()[:12]
                email = f"apikey-{digest}@codex.local"
            else:
                return {**absent, "auth_state": "identity_missing"}
        subject = compute_subject(email, org)
        return {
            "logged_in": True,
            "email": email,
            "organization": org,
            "plan": plan,
            "usage_status": "unavailable",
            "auth_state": "credentials_present",
            "subject": subject,
        }

    def copy_auth(self, source_dir, target_dir):
        selected, _ = read_regular_json(self.auth_candidates(source_dir))
        if selected is None:
            return False
        atomic_copy_private(selected, target_dir / "auth.json")
        return True

    def desktop_emails(self):
        return []

    def login_command(self, binary, extra_args, scoped=False):
        _ = scoped
        cmd = [_runtime["path"](binary), "-c", 'cli_auth_credentials_store="file"', "login"]
        if "--device-auth" in extra_args:
            cmd.append("--device-auth")
        return cmd

    def run_command(self, binary, extra_args, is_default):
        has_notify = any("notify" in str(arg) for arg in extra_args)
        notify_flags = [] if has_notify else ["-c", 'notify=["/usr/local/bin/cage-provider-account","codex","notify"]']
        if is_default:
            return [_runtime["path"](binary), *notify_flags, *extra_args]
        return [_runtime["path"](binary), "-c", 'cli_auth_credentials_store="file"', *notify_flags, *extra_args]


ADAPTERS = {
    "antigravity": AntigravityAdapter(),
    "codex": CodexAdapter(),
}


def handle_notify(provider_id, rest):
    conn = valid_notify_connection(os.environ.get(NOTIFY_ENV_VAR, ""))
    term_id = valid_terminal_id(os.environ.get(TERMINAL_ID_ENV_VAR, ""))
    if provider_id == "antigravity":
        raw = sys.stdin.read(256 << 10)
        sys.stdout.write('{"decision":"allow"}\n')
        sys.stdout.flush()
        try:
            payload = json.loads(raw) if raw.strip() else {}
        except ValueError:
            payload = {}
        if not isinstance(payload, dict):
            payload = {}
        sid = ""
        for key in ("conversation_id", "conversationId", "session_id", "sessionId", "trajectory_id", "trajectoryId"):
            sid = valid_provider_session_id("antigravity", payload.get(key, ""))
            if sid:
                break
        if not sid and (conn or term_id):
            acct_id = os.environ.get("CAGE_PROVIDER_ACCOUNT_ID") or DEFAULT
            try:
                cfg = ADAPTERS["antigravity"].config_dir(account_base("antigravity", acct_id))
            except ValueError:
                cfg = Path(os.environ.get("ANTIGRAVITY_GEMINI_DIR") or (user_home() / ".gemini"))
            claimed = claimed_terminal_sessions("antigravity", term_id)
            for cand in list_workspace_sessions("antigravity", cfg, os.getcwd()):
                if cand not in claimed:
                    sid = cand
                    break
        if sid:
            record_terminal_session("antigravity", term_id, conn, sid)
        if not conn:
            return 0
        reason = payload.get("terminationReason")
        if reason not in ("USER_CANCELED", "ERROR"):
            record_channel_event(conn, "turn_completed", cli_id=sid)
        return 0
    if provider_id == "codex":
        raw = rest[0] if rest else ""
        try:
            payload = json.loads(raw) if raw.strip() else {}
        except ValueError:
            payload = {}
        if not isinstance(payload, dict):
            payload = {}
        sid = ""
        for key in ("thread-id", "thread_id", "session_id", "sessionId", "conversation_id"):
            sid = valid_provider_session_id("codex", payload.get(key, ""))
            if sid:
                break
        if sid:
            record_terminal_session("codex", term_id, conn, sid)
        if not conn:
            return 0
        kind = str(payload.get("type") or "")
        if "approval" in kind or "permission" in kind:
            record_channel_event(conn, "approval_requested", cli_id=sid)
        elif not kind or "turn-complete" in kind or "turn_complete" in kind:
            record_channel_event(conn, "turn_completed", cli_id=sid)
        return 0
    return 0


LOGIN_URL_RE = re.compile(
    r"https://(?:"
    r"accounts\.google\.com/(?:o/oauth2|AccountChooser)[^\s\"'<>\\\x00-\x1f]+|"
    r"antigravity\.google(?:\.com)?/(?:login|auth|oauth)[^\s\"'<>\\\x00-\x1f]*|"
    r"(?:auth|auth0)\.openai\.com/(?:oauth/authorize|authorize|u/login|codex/device|device|activate)[^\s\"'<>\\\x00-\x1f]*|"
    r"(?:chatgpt\.com|chat\.openai\.com)/(?:activate|auth/)[^\s\"'<>\\\x00-\x1f]*"
    r")",
    re.IGNORECASE,
)
ANSI_ESCAPE_RE = re.compile(r"\x1b(?:\[[0-9;?]*[ -/]*[@-~]|\][^\x07\x1b]*(?:\x07|\x1b\\)|[@-Z\\-_])")


def extract_stream_login_url(raw_text):
    cleaned = ANSI_ESCAPE_RE.sub("", raw_text)
    # A newline may separate the URL from status text; joining it can silently
    # change OAuth state/code values. PTY soft wraps are not URL characters.
    match = LOGIN_URL_RE.search(cleaned)
    return match.group(0) if match else ""


def run_login_pty_with_auto_open(cmd, env, launcher_path, opened_marker, callback_poll_cmd=None):
    import errno
    import fcntl
    import pty
    import select
    import termios
    import threading
    import tty

    stdin_fd = sys.stdin.fileno()
    stdout_fd = sys.stdout.fileno()
    master_fd, slave_fd = pty.openpty()

    def sync_winsize(_signum=None, _frame=None):
        try:
            winsize = fcntl.ioctl(stdin_fd, termios.TIOCGWINSZ, b"\x00" * 8)
            fcntl.ioctl(master_fd, termios.TIOCSWINSZ, winsize)
        except OSError:
            pass

    sync_winsize()
    old_winch = signal.getsignal(signal.SIGWINCH) if hasattr(signal, "SIGWINCH") else None
    if hasattr(signal, "SIGWINCH"):
        signal.signal(signal.SIGWINCH, sync_winsize)
    old_attrs = termios.tcgetattr(stdin_fd)
    proc = None
    buf_lock = threading.Lock()
    state = {"text": "", "scheduled": False, "callback_sent": False, "closed": False}

    def trigger_fallback():
        time.sleep(0.35)
        if opened_marker.exists() or state["closed"]:
            return
        with buf_lock:
            url = extract_stream_login_url(state["text"])
        if not url or opened_marker.exists() or state["closed"]:
            return
        try:
            opened_marker.write_bytes(b"1")
        except OSError:
            pass
        subprocess.run(
            [launcher_path, url],
            env=os.environ,
            timeout=12,
            check=False,
            stdin=subprocess.DEVNULL,
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
        )

    def poll_browser_callback():
        while not state["closed"] and not state["callback_sent"]:
            time.sleep(0.75)
            if state["closed"] or state["callback_sent"] or not opened_marker.exists():
                continue
            if proc is not None and proc.poll() is not None:
                return
            try:
                res = subprocess.run(
                    callback_poll_cmd,
                    env=os.environ,
                    timeout=5,
                    check=False,
                    stdin=subprocess.DEVNULL,
                    stdout=subprocess.PIPE,
                    stderr=subprocess.DEVNULL,
                    text=True,
                )
            except (OSError, subprocess.SubprocessError):
                continue
            landed = (res.stdout or "").strip()
            if landed.startswith("https://antigravity.google") and "code=" in landed and not state["callback_sent"]:
                state["callback_sent"] = True
                try:
                    os.write(master_fd, (landed + "\n").encode("utf-8"))
                except OSError:
                    pass
                return

    try:
        tty.setraw(stdin_fd)
        proc = subprocess.Popen(
            cmd,
            stdin=slave_fd,
            stdout=slave_fd,
            stderr=slave_fd,
            cwd=user_home(),
            env=env,
            close_fds=True,
        )
        os.close(slave_fd)
        slave_fd = -1
        if callback_poll_cmd:
            threading.Thread(target=poll_browser_callback, daemon=True).start()
        watch_fds = [stdin_fd, master_fd]
        while master_fd in watch_fds:
            try:
                ready, _, _ = select.select(watch_fds, [], [], 0.25)
            except InterruptedError:
                continue
            if master_fd in ready:
                try:
                    data = os.read(master_fd, 4096)
                except OSError as exc:
                    if exc.errno == errno.EIO:
                        data = b""
                    else:
                        raise
                if not data:
                    watch_fds.remove(master_fd)
                else:
                    os.write(stdout_fd, data)
                    if not state["scheduled"] and not opened_marker.exists():
                        with buf_lock:
                            state["text"] = (state["text"] + data.decode("utf-8", "ignore"))[-16384:]
                            found = extract_stream_login_url(state["text"])
                        if found:
                            state["scheduled"] = True
                            threading.Thread(target=trigger_fallback, daemon=True).start()
            if stdin_fd in ready:
                try:
                    user_in = os.read(stdin_fd, 1024)
                except OSError:
                    user_in = b""
                if not user_in:
                    watch_fds.remove(stdin_fd)
                else:
                    state["callback_sent"] = True
                    os.write(master_fd, user_in)
        return proc.wait()
    finally:
        state["closed"] = True
        if slave_fd >= 0:
            try:
                os.close(slave_fd)
            except OSError:
                pass
        try:
            os.close(master_fd)
        except OSError:
            pass
        try:
            termios.tcsetattr(stdin_fd, termios.TCSADRAIN, old_attrs)
        except OSError:
            pass
        if hasattr(signal, "SIGWINCH") and old_winch is not None:
            signal.signal(signal.SIGWINCH, old_winch)
        if proc is not None and proc.poll() is None:
            proc.terminate()
            try:
                proc.wait(timeout=2)
            except subprocess.TimeoutExpired:
                proc.kill()


def run_provider(args):
    if len(args) >= 2 and args[1] == "notify":
        return handle_notify(args[0], args[2:])
    if len(args) < 3:
        raise ValueError("usage: cage-provider-account <provider> <mode> <account_id> [args...]")
    provider_id, mode, account_id, *rest = args
    adapter = ADAPTERS.get(provider_id)
    if adapter is None:
        raise ValueError(f"unsupported provider: {provider_id}")

    base = account_base(provider_id, account_id)
    if account_id == DEFAULT and mode == "login":
        raise ValueError("new logins require a private profile")

    config_dir = adapter.config_dir(base)
    original_umask = os.umask(0o077)
    if account_id != DEFAULT:
        adapter.ensure_profile(config_dir)
    elif mode == "run" and hasattr(adapter, "ensure_hooks"):
        try:
            adapter.ensure_hooks(config_dir)
        except OSError:
            pass

    if mode == "share":
        return 0

    env = adapter.environment(base)
    if mode == "snapshot":
        if account_id == DEFAULT:
            if any(env.get(key) for key in adapter.external_env_keys) or not config_dir.is_dir():
                print(json.dumps({"logged_in": False, "auth_state": "signed_out", "desktop_emails": adapter.desktop_emails()}))
                return 0
            summary = adapter.identity(config_dir, env)
            print(json.dumps({**summary, "desktop_emails": adapter.desktop_emails()}))
            return 0
        print(json.dumps(adapter.identity(config_dir, env)))
        return 0

    if mode in ("archive-login", "recover-login"):
        if len(rest) != 2 or account_id == DEFAULT or account_id == rest[0]:
            raise ValueError("invalid account recovery")
        source_id, expected = rest
        current = adapter.identity(config_dir, env)
        if current.get("subject") == expected and expected:
            print(json.dumps(current))
            return 0
        source_base = account_base(provider_id, source_id)
        source_dir = adapter.config_dir(source_base)
        authenticated = adapter.identity(source_dir, env)
        if not expected or authenticated.get("subject") != expected:
            print(json.dumps({"logged_in": False}))
            return 0
        if not adapter.copy_auth(source_dir, config_dir):
            print(json.dumps({"logged_in": False}))
            return 0
        recovered = adapter.identity(config_dir, env)
        print(json.dumps(recovered if recovered.get("subject") == expected else {"logged_in": False}))
        return 0

    binary = os.environ.get("CAGE_CLI_BINARY") if os.name == "nt" else shutil.which(adapter.binary_name, path=env["PATH"])
    if not binary:
        print(f"尚未安装 {adapter.name} CLI ({adapter.binary_name})，请在设置中更新环境工具后重试。", file=sys.stderr)
        return 1

    owner = _controller_context.get("CAGE_LOGIN_OWNER", "")
    owner_args = ["--account-id", owner] if ACCOUNT_ID_RE.fullmatch(owner) else []
    scoped = _controller_context.get("CAGE_ACCOUNT_BROWSER")

    if mode == "login":
        opener_dir = None
        launcher_path = None
        opened_marker = None
        if os.name != "nt":
            launcher_path = _runtime["launcher"](scoped or OPEN, executable=True, arguments=[*owner_args, "--login-id", account_id])
            opener_dir = tempfile.mkdtemp(prefix="xdg-")
            os.chmod(opener_dir, PRIVATE_MODE)
            opened_marker = Path(opener_dir) / "opened"
            quoted_launcher = "'" + str(launcher_path).replace("'", "'\\''") + "'"
            quoted_marker = "'" + str(opened_marker).replace("'", "'\\''") + "'"
            wrapper_path = Path(opener_dir) / "xdg-open"
            wrapper_path.write_text(
                f"#!/bin/sh\n: > {quoted_marker} 2>/dev/null || true\nexec {quoted_launcher} \"$@\"\n",
                encoding="utf-8",
            )
            os.chmod(wrapper_path, 0o700)
            env["BROWSER"] = str(wrapper_path)
            if sys.platform != "darwin":
                env["PATH"] = opener_dir + (os.pathsep + env["PATH"] if env.get("PATH") else "")

        def interrupted(_signum, _frame):
            raise KeyboardInterrupt()

        if hasattr(signal, "SIGHUP"):
            signal.signal(signal.SIGHUP, interrupted)
        signal.signal(signal.SIGTERM, interrupted)
        try:
            cmd = adapter.login_command(binary, rest, scoped=bool(scoped))
            if os.name != "nt" and launcher_path and opened_marker and sys.stdin.isatty() and sys.stdout.isatty():
                poll_cmd = (
                    [scoped or OPEN, *owner_args, "--login-id", account_id, "--poll-login-callback"]
                    if provider_id == "antigravity"
                    else None
                )
                return run_login_pty_with_auto_open(cmd, env, launcher_path, opened_marker, callback_poll_cmd=poll_cmd)
            return subprocess.call(cmd, cwd=user_home(), env=env)
        finally:
            if opener_dir:
                shutil.rmtree(opener_dir, ignore_errors=True)
            if os.name != "nt":
                finish = [scoped or OPEN, *owner_args, "--login-id", account_id, "--finish-login"]
                subprocess.run(
                    finish,
                    env=os.environ,
                    timeout=8,
                    check=False,
                    stdin=subprocess.DEVNULL,
                    stdout=subprocess.DEVNULL,
                    stderr=subprocess.DEVNULL,
                )

    if mode == "run":
        if os.name != "nt":
            run_launcher = _runtime["launcher"](scoped or OPEN, executable=True, arguments=owner_args, login_config=config_dir)
            env["BROWSER"] = run_launcher
            if sys.platform != "darwin" and (owner_args or scoped):
                xdg_dir = Path(_runtime["path"](config_dir)).parent / "xdg-bin" if "path" in _runtime else config_dir / ".xdg-bin"
                try:
                    xdg_dir.mkdir(mode=PRIVATE_MODE, parents=True, exist_ok=True)
                    xdg_shim = xdg_dir / "xdg-open"
                    if xdg_shim.is_symlink() or xdg_shim.exists():
                        xdg_shim.unlink()
                    xdg_shim.symlink_to(run_launcher)
                    env["PATH"] = str(xdg_dir) + (os.pathsep + env["PATH"] if env.get("PATH") else "")
                except OSError:
                    pass
        notify_conn = valid_notify_connection(_controller_context.get("CAGE_NOTIFY_CONNECTION", ""))
        if notify_conn:
            env[NOTIFY_ENV_VAR] = notify_conn
        term_id = valid_terminal_id(_controller_context.get("CAGE_TERMINAL_ID", ""))
        if term_id:
            env[TERMINAL_ID_ENV_VAR] = term_id
        expected = _controller_context.get("CAGE_PROVIDER_SUBJECT")
        if expected:
            reading = adapter.identity(config_dir, env)
            if reading.get("subject") != expected:
                print(f"此终端所选 {adapter.name} 账号身份已变化或尚未登录，请重新授权后再试。", file=sys.stderr)
                return 1
        if hasattr(adapter, "trust_workspace"):
            try:
                adapter.trust_workspace(config_dir, os.getcwd())
            except OSError:
                pass
        resumed_id = extract_explicit_resume_id(provider_id, rest)
        if resumed_id:
            record_terminal_session(provider_id, term_id, notify_conn, resumed_id)
        elif term_id or notify_conn:
            start_session_watcher(provider_id, config_dir, os.getcwd(), term_id, notify_conn, os.getpid())
        os.umask(original_umask)
        cmd = adapter.run_command(binary, rest, account_id == DEFAULT)
        if os.name == "nt":
            return subprocess.call(cmd, env=env)
        os.execve(cmd[0], cmd, env)

    raise ValueError("unknown operation")


def entrypoint(args):
    try:
        return run_provider(args)
    except KeyboardInterrupt:
        return 130
    except (OSError, ValueError, subprocess.SubprocessError, TypeError):
        print("准备终端或启动官方 CLI 失败，请检查目录权限并同步环境组件后重试；现有登录信息已保留。", file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(entrypoint(sys.argv[1:]))
