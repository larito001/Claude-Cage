#!/usr/bin/env python3
"""Isolated guest CLI account runner and local credential inspector for non-Claude providers.

Follows the exact same isolation model as cage-claude-account:
- Each managed account owns ~/.local/share/cage/<provider>-accounts/<id>/config (0700).
- Host/global API keys and external auth env vars are stripped in managed sessions.
- File-backed credential storage is enforced so no shared keyring can cross-contaminate accounts.
- OAuth logins open through Cage's scoped LoginBrowser (--account-id / --login-id) inside
  the account's dedicated network namespace when configured, never touching Browser Spaces or Desktops.
- Raw tokens, paths, and callback URLs are never printed to stdout/stderr.
"""
import base64
from datetime import datetime
import hashlib
import json
import math
import os
from pathlib import Path
import re
import runpy
import shutil
import signal
import subprocess
import sys
import tempfile
import time
import uuid

DEFAULT = "default"
PRIVATE_MODE = 0o700
ACCOUNT_ID_RE = re.compile(r"^[0-9a-f]{24}$")
EMAIL_RE = re.compile(r"^[^\s@\x00-\x1f]+@[^\s@\x00-\x1f]+\.[^\s@\x00-\x1f]+$")
OPEN = "/usr/local/libexec/cage-cli/bin/cage-open" if sys.platform == "darwin" else "/usr/local/bin/cage-open"
CLI_CHANNEL_ROOT = Path(os.environ.get("CAGE_CLI_STATE_ROOT") or (Path.home() / ".local/share/cage/cli-channels"))
NOTIFY_ENV_VAR = "__WORKBENCH_NOTIFY_CHANNEL"

_controller_context = {
    key: os.environ.pop(key)
    for key in (
        "CAGE_LOGIN_OWNER",
        "CAGE_ACCOUNT_BROWSER",
        "CAGE_PROVIDER_SUBJECT",
        "CAGE_PROVIDER_ACCOUNT_ID",
        "CAGE_NOTIFY_CONNECTION",
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
    root = Path.home() / ".local/share/cage" / f"{provider_id}-accounts"
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


def record_channel_event(conn_id, event_kind):
    conn = valid_notify_connection(conn_id)
    if not conn or event_kind not in ("turn_completed", "approval_requested"):
        return
    folder = CLI_CHANNEL_ROOT / conn
    folder.mkdir(mode=PRIVATE_MODE, parents=True, exist_ok=True)
    state_path = folder / "state.json"

    def _update():
        try:
            state = json.loads(state_path.read_text(encoding="utf-8"))
            if not isinstance(state, dict):
                state = {}
        except (OSError, ValueError):
            state = {}
        state["updated_at"] = time.time()
        if event_kind == "turn_completed":
            prev = state.get("turn_done")
            state["turn_done"] = (prev if isinstance(prev, int) and prev > 0 else 0) + 1
        elif event_kind == "approval_requested":
            approvals = state.get("approvals")
            if not isinstance(approvals, list):
                approvals = []
            approvals.append({"id": uuid.uuid4().hex[:16]})
            state["approvals"] = approvals[-32:]
        atomic_write_json(state_path, state)

    if os.name == "nt":
        _update()
        return
    import fcntl
    with (folder / "lock").open("a") as lock:
        fcntl.flock(lock, fcntl.LOCK_EX)
        _update()


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
        return Path.home() / ".gemini" if base.name == DEFAULT else base / "config"

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
        cli_dir.mkdir(mode=PRIVATE_MODE, parents=True, exist_ok=True)
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
        self.ensure_hooks(config_dir)

    def environment(self, base):
        env = os.environ.copy()
        if os.name != "nt":
            for ssh_key in ("SSH_CLIENT", "SSH_CONNECTION", "SSH_TTY"):
                env.pop(ssh_key, None)
        parts = [p for p in env.get("PATH", os.defpath).split(os.pathsep) if p and p != self.shim_dir]
        env["PATH"] = os.pathsep.join([str(Path.home() / ".local/bin"), *parts])
        if base.name == DEFAULT:
            env.pop("ANTIGRAVITY_GEMINI_DIR", None)
            return _runtime["environment"](env)
        env["ANTIGRAVITY_GEMINI_DIR"] = str(self.config_dir(base))
        env["AGY_CLI_FORCE_FILE_TOKEN_STORAGE"] = "1"
        for key in self.external_env_keys:
            env.pop(key, None)
        return _runtime["environment"](env)

    def auth_candidates(self, config_dir):
        return [
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
        return {
            "logged_in": True,
            "email": email,
            "organization": "",
            "plan": "antigravity",
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
        return True

    def desktop_emails(self):
        home = Path.home()
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
        return Path.home() / ".codex" if base.name == DEFAULT else base / "config"

    def ensure_profile(self, config_dir):
        config_dir.mkdir(mode=PRIVATE_MODE, parents=True, exist_ok=True)
        toml_file = config_dir / "config.toml"
        key_line = 'cli_auth_credentials_store = "file"'
        try:
            existing = toml_file.read_text(encoding="utf-8") if toml_file.is_file() else ""
        except OSError:
            existing = ""
        if "cli_auth_credentials_store" not in existing:
            prefix = key_line + "\n"
            updated = prefix + existing
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
        cmd = [_runtime["path"](binary), "-c", 'cli_auth_credentials_store="file"', "login"]
        if "--device-auth" in extra_args or scoped:
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
    if provider_id == "antigravity":
        raw = sys.stdin.read(256 << 10)
        sys.stdout.write('{"decision":"allow"}\n')
        sys.stdout.flush()
        if not conn:
            return 0
        try:
            payload = json.loads(raw) if raw.strip() else {}
        except ValueError:
            payload = {}
        reason = payload.get("terminationReason") if isinstance(payload, dict) else ""
        if reason not in ("USER_CANCELED", "ERROR"):
            record_channel_event(conn, "turn_completed")
        return 0
    if provider_id == "codex":
        if not conn:
            return 0
        raw = rest[0] if rest else ""
        try:
            payload = json.loads(raw) if raw.strip() else {}
        except ValueError:
            payload = {}
        kind = str(payload.get("type") or "") if isinstance(payload, dict) else ""
        if "approval" in kind or "permission" in kind:
            record_channel_event(conn, "approval_requested")
        elif not kind or "turn-complete" in kind or "turn_complete" in kind:
            record_channel_event(conn, "turn_completed")
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
    collapsed = re.sub(r"\r?\n[ \t]*", "", cleaned)
    match = LOGIN_URL_RE.search(collapsed) or LOGIN_URL_RE.search(cleaned)
    return match.group(0) if match else ""


def run_login_pty_with_auto_open(cmd, env, launcher_path, opened_marker):
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
    state = {"text": "", "scheduled": False}

    def trigger_fallback():
        time.sleep(0.35)
        if opened_marker.exists():
            return
        with buf_lock:
            url = extract_stream_login_url(state["text"])
        if not url or opened_marker.exists():
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

    try:
        tty.setraw(stdin_fd)
        proc = subprocess.Popen(
            cmd,
            stdin=slave_fd,
            stdout=slave_fd,
            stderr=slave_fd,
            cwd=Path.home(),
            env=env,
            close_fds=True,
        )
        os.close(slave_fd)
        slave_fd = -1
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
                    os.write(master_fd, user_in)
        return proc.wait()
    finally:
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
                return run_login_pty_with_auto_open(cmd, env, launcher_path, opened_marker)
            return subprocess.call(cmd, cwd=Path.home(), env=env)
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
        expected = _controller_context.get("CAGE_PROVIDER_SUBJECT")
        if expected:
            reading = adapter.identity(config_dir, env)
            if reading.get("subject") != expected:
                print(f"此终端所选 {adapter.name} 账号身份已变化或尚未登录，请重新授权后再试。", file=sys.stderr)
                return 1
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
