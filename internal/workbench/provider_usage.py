#!/usr/bin/env python3
"""Compliant official-CLI quota and local session usage inspector for Codex and Antigravity.

- Never uses reverse proxies, MITM interception, or undocumented HTTP endpoints.
- Queries only the official installed CLI binaries (`codex app-server` JSON-RPC
  `account/rateLimits/read` and `agy` `/quota` in a local PTY) inside the
  account's isolated profile directory and network namespace.
- Verifies account subject hash before and after every live query so a changed
  login can never attach its quota to another account record.
"""
import base64
from datetime import datetime
import errno
import hashlib
import json
import math
import os
from pathlib import Path
import re
import select
import shutil
import struct
import subprocess
import sys
import tempfile
import time

if os.name == "nt":
    fcntl = None
    pty = None
    termios = None
else:
    import fcntl
    import pty
    import termios

DEFAULT = "default"
PRIVATE_MODE = 0o700
ACCOUNT_ID_RE = re.compile(r"^[0-9a-f]{24}$")
SUBJECT_RE = re.compile(r"^[0-9a-f]{64}$")
EMAIL_RE = re.compile(r"^[^\s@\x00-\x1f]+@[^\s@\x00-\x1f]+\.[^\s@\x00-\x1f]+$")
ANSI_ESCAPE_RE = re.compile(r"\x1b(?:\[[0-9;?]*[ -/]*[@-~]|\][^\x07\x1b]*(?:\x07|\x1b\\)|[@-Z\\-_])")


def user_home():
    home = Path(os.environ.get("CAGE_TEST_HOME") or Path.home())
    if os.name != "nt" and "CAGE_TEST_HOME" not in os.environ:
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


def clean_environment(env):
    return {k: v for k, v in env.items() if not k.upper().startswith("CAGE_")}


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


def parse_iso_timestamp(value):
    if not isinstance(value, str) or not value.strip():
        return None
    try:
        dt = datetime.fromisoformat(value.strip().replace("Z", "+00:00"))
        if dt.tzinfo is None:
            return None
        return int(dt.timestamp())
    except (ValueError, OverflowError):
        return None


def ordinary_path(path):
    return not path.is_symlink() and not (hasattr(path, "is_junction") and path.is_junction())


def read_regular_json(candidates):
    for candidate in candidates:
        if not candidate.exists():
            continue
        try:
            if not ordinary_path(candidate) or not ordinary_path(candidate.parent) or not candidate.is_file():
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


def account_base(provider_id, account_id):
    if provider_id not in ("codex", "antigravity"):
        raise ValueError("unsupported provider")
    if account_id != DEFAULT and not ACCOUNT_ID_RE.fullmatch(account_id or ""):
        raise ValueError("invalid account identifier")
    root = user_home() / ".local/share/cage" / f"{provider_id}-accounts"
    root.mkdir(mode=PRIVATE_MODE, parents=True, exist_ok=True)
    base = root / account_id
    base.mkdir(mode=PRIVATE_MODE, parents=True, exist_ok=True)
    return base


def config_dir_for(provider_id, base):
    if provider_id == "codex":
        return user_home() / ".codex" if base.name == DEFAULT else base / "config"
    return user_home() / ".gemini" if base.name == DEFAULT else base / "config"


def codex_environment(base):
    env = os.environ.copy()
    if os.name != "nt":
        for ssh_key in ("SSH_CLIENT", "SSH_CONNECTION", "SSH_TTY"):
            env.pop(ssh_key, None)
    shim_dir = "/usr/local/lib/cage-codex-bin"
    parts = [p for p in env.get("PATH", os.defpath).split(os.pathsep) if p and p != shim_dir]
    env["PATH"] = os.pathsep.join([str(user_home() / ".local/bin"), *parts])
    if base.name == DEFAULT:
        env.pop("CODEX_HOME", None)
        return clean_environment(env)
    env["CODEX_HOME"] = str(config_dir_for("codex", base))
    for key in ("OPENAI_API_KEY", "OPENAI_BASE_URL", "CODEX_API_KEY", "CODEX_ACCESS_TOKEN"):
        env.pop(key, None)
    return clean_environment(env)


def antigravity_environment(base):
    env = os.environ.copy()
    if os.name != "nt":
        for ssh_key in ("SSH_CLIENT", "SSH_CONNECTION", "SSH_TTY"):
            env.pop(ssh_key, None)
    real = user_home()
    shim_dir = "/usr/local/lib/cage-antigravity-bin"
    parts = [p for p in env.get("PATH", os.defpath).split(os.pathsep) if p and p != shim_dir]
    env["PATH"] = os.pathsep.join([str(real / ".local/bin"), *parts])
    env["TERM"] = "xterm-256color"
    if base.name == DEFAULT:
        if os.name != "nt":
            env["HOME"] = str(real)
        env.pop("ANTIGRAVITY_GEMINI_DIR", None)
        return clean_environment(env)
    config_dir = config_dir_for("antigravity", base)
    home_dir = base / "home"
    if os.name != "nt":
        home_dir.mkdir(mode=PRIVATE_MODE, parents=True, exist_ok=True)
        config_dir.mkdir(mode=PRIVATE_MODE, parents=True, exist_ok=True)
        gemini_link = home_dir / ".gemini"
        if not gemini_link.exists() and not gemini_link.is_symlink():
            try:
                gemini_link.symlink_to(config_dir, target_is_directory=True)
            except OSError:
                pass
        env["HOME"] = str(home_dir)
    env["ANTIGRAVITY_GEMINI_DIR"] = str(config_dir)
    env["AGY_CLI_FORCE_FILE_TOKEN_STORAGE"] = "1"
    for key in (
        "GEMINI_API_KEY",
        "GOOGLE_API_KEY",
        "GOOGLE_GEMINI_BASE_URL",
        "GOOGLE_APPLICATION_CREDENTIALS",
        "GOOGLE_CLOUD_PROJECT",
        "GOOGLE_CLOUD_LOCATION",
        "GOOGLE_CLOUD_QUOTA_PROJECT",
        "GOOGLE_GENAI_USE_VERTEXAI",
        "AGY_ADC_AUTH",
    ):
        env.pop(key, None)
    return clean_environment(env)


def codex_identity(config_dir, env, is_default=False):
    absent = {"logged_in": False, "auth_state": "signed_out", "subject": ""}
    if is_default and any(env.get(k) for k in ("OPENAI_API_KEY", "OPENAI_BASE_URL", "CODEX_API_KEY", "CODEX_ACCESS_TOKEN")):
        return absent
    _, data = read_regular_json([config_dir / "auth.json"])
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
    is_api_key = False
    if not EMAIL_RE.fullmatch(email) or len(email) > 254:
        if isinstance(api_key, str) and len(api_key.strip()) >= 8:
            digest = hashlib.sha256(api_key.strip().encode("utf-8")).hexdigest()[:12]
            email = f"apikey-{digest}@codex.local"
            is_api_key = True
        else:
            return {**absent, "auth_state": "identity_missing"}
    return {
        "logged_in": True,
        "email": email,
        "organization": org,
        "plan": plan,
        "auth_state": "credentials_present",
        "subject": compute_subject(email, org),
        "api_key_mode": is_api_key,
    }


def antigravity_identity(config_dir, env, is_default=False):
    absent = {"logged_in": False, "auth_state": "signed_out", "subject": ""}
    ext_keys = (
        "GEMINI_API_KEY",
        "GOOGLE_API_KEY",
        "GOOGLE_GEMINI_BASE_URL",
        "GOOGLE_APPLICATION_CREDENTIALS",
        "GOOGLE_CLOUD_PROJECT",
        "GOOGLE_GENAI_USE_VERTEXAI",
        "AGY_ADC_AUTH",
    )
    if is_default and any(env.get(k) for k in ext_keys):
        return absent
    candidates_paths = [
        config_dir / "antigravity-cli" / "antigravity-oauth-token",
        config_dir / "antigravity-oauth-token",
        config_dir / "auth.json",
        config_dir / "antigravity-cli" / "auth.json",
        config_dir / "config" / "auth.json",
    ]
    _, data = read_regular_json(candidates_paths)
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
    auth_method = field("auth_method", "authMethod").lower()
    plan = "enterprise" if auth_method == "enterprise" else "antigravity"
    return {
        "logged_in": True,
        "email": email,
        "organization": "",
        "plan": plan,
        "auth_state": "refresh_required" if expired or not access else "credentials_present",
        "subject": compute_subject(email, ""),
    }


def valid_window(data, now, allow_expired=False):
    if not isinstance(data, dict):
        return None
    used = data.get("used_percentage")
    reset = data.get("resets_at", 0)
    if isinstance(used, bool) or not isinstance(used, (int, float)) or not math.isfinite(used) or not 0 <= used <= 100:
        return None
    if reset is None:
        reset = 0
    if isinstance(reset, bool) or not isinstance(reset, int) or reset < 0:
        return None
    if not allow_expired and reset != 0 and reset <= now:
        return None
    clean_used = round(float(used), 1)
    if clean_used.is_integer():
        clean_used = int(clean_used)
    out = {"used_percentage": clean_used, "resets_at": reset}
    seen = data.get("observed_at")
    if not isinstance(seen, bool) and isinstance(seen, int) and 0 < seen <= now + 60:
        out["observed_at"] = min(seen, now)
    return out


def valid_group(group, now, default_seen=None, allow_expired=False):
    if not isinstance(group, dict):
        return None
    gid = group.get("id")
    label = group.get("label")
    if not isinstance(gid, str) or not re.fullmatch(r"[a-z0-9_-]{1,32}", gid):
        return None
    if not isinstance(label, str) or not 1 <= len(label.strip()) <= 48:
        return None
    entry = {"id": gid, "label": label.strip()}
    has_any = False
    for key in ("five_hour", "seven_day"):
        win = valid_window(group.get(key), now, allow_expired=allow_expired)
        if win:
            if "observed_at" not in win and default_seen:
                win["observed_at"] = default_seen
            entry[key] = win
            has_any = True
    return entry if has_any else None


def read_quota(base):
    quota_path = base / "quota.json"
    _, stored = read_regular_json([quota_path])
    if not isinstance(stored, dict):
        return None
    digest = stored.get("identity", "")
    seen = stored.get("observed_at")
    now = int(time.time())
    if not isinstance(digest, str) or not SUBJECT_RE.fullmatch(digest) or isinstance(seen, bool) or not isinstance(seen, int) or not 0 < seen <= now + 60:
        return None
    seen = min(seen, now)
    result = {}
    for key in ("five_hour", "seven_day"):
        win = valid_window(stored.get(key), now, allow_expired=True)
        if win:
            win.setdefault("observed_at", seen)
            result[key] = win
    groups = []
    for raw_group in stored.get("groups") or ():
        clean = valid_group(raw_group, now, default_seen=seen, allow_expired=True)
        if clean:
            groups.append(clean)
    if groups:
        result["groups"] = groups[:6]
    if stored.get("single_window") in ("five_hour", "seven_day"):
        result["single_window"] = stored["single_window"]
    if not result:
        return None
    result["identity"] = digest
    result["observed_at"] = seen
    if isinstance(stored.get("plan"), str) and 1 <= len(stored["plan"]) <= 48:
        result["plan"] = stored["plan"]
    return result


def save_quota(base, identity_subject, windows, groups=None, single_window="", plan="", observed_at=None, deadline=None):
    if not SUBJECT_RE.fullmatch(identity_subject or ""):
        return None
    now = int(time.time())
    stamp = min(int(observed_at), now) if isinstance(observed_at, int) and observed_at > 0 else now
    base.mkdir(mode=PRIVATE_MODE, parents=True, exist_ok=True)

    def _write():
        previous = read_quota(base) or {}
        if previous.get("identity") != identity_subject:
            previous = {}
        merged = {}
        changed = False
        for key in ("five_hour", "seven_day"):
            old = previous.get(key)
            new = valid_window((windows or {}).get(key), now)
            if new:
                new["observed_at"] = new.get("observed_at") or stamp
                if old and old.get("observed_at", 0) > new["observed_at"]:
                    merged[key] = old
                else:
                    merged[key] = new
                    if old != new:
                        changed = True
            elif old:
                merged[key] = old

        clean_groups = []
        for g in groups or ():
            cg = valid_group(g, now, default_seen=stamp)
            if cg:
                clean_groups.append(cg)
        if clean_groups:
            merged["groups"] = clean_groups[:6]
            if previous.get("groups") != merged["groups"]:
                changed = True
        elif previous.get("groups"):
            merged["groups"] = previous["groups"]

        if single_window in ("five_hour", "seven_day"):
            merged["single_window"] = single_window
            if previous.get("single_window") != single_window:
                changed = True
        elif previous.get("single_window") in ("five_hour", "seven_day") and "five_hour" not in (windows or {}):
            merged["single_window"] = previous["single_window"]

        clean_plan = (plan or "").strip()
        if 1 <= len(clean_plan) <= 48:
            merged["plan"] = clean_plan
            if previous.get("plan") != clean_plan:
                changed = True
        elif previous.get("plan"):
            merged["plan"] = previous["plan"]

        if not any(k in merged for k in ("five_hour", "seven_day", "groups")):
            return previous or None
        if not changed and previous:
            return previous

        merged["identity"] = identity_subject
        merged["observed_at"] = max(stamp, int(previous.get("observed_at") or 0))
        with tempfile.NamedTemporaryFile(mode="w", encoding="utf-8", dir=base, delete=False) as tmp:
            json.dump(merged, tmp, separators=(",", ":"), ensure_ascii=False)
            tmp.flush()
            os.fsync(tmp.fileno())
            tmp_name = tmp.name
        try:
            if os.name != "nt":
                os.chmod(tmp_name, 0o600)
            os.replace(tmp_name, base / "quota.json")
        except BaseException:
            Path(tmp_name).unlink(missing_ok=True)
            raise
        return merged

    if fcntl is None:
        return _write()
    with (base / "quota.lock").open("a") as lock:
        if deadline is None:
            fcntl.flock(lock, fcntl.LOCK_EX)
        else:
            while True:
                try:
                    fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
                    break
                except BlockingIOError:
                    if time.monotonic() >= deadline:
                        raise TimeoutError("quota lock busy")
                    time.sleep(0.05)
        return _write()


def classify_codex_window(raw_win, fallback_key, now, observed_at=None):
    if not isinstance(raw_win, dict):
        return None, None
    used = raw_win.get("usedPercent")
    if used is None:
        used = raw_win.get("used_percent")
    resets = raw_win.get("resetsAt")
    if resets is None:
        resets = raw_win.get("resets_at")
    if resets is None:
        resets = 0
    if isinstance(resets, float) and math.isfinite(resets):
        resets = int(resets)
    mins = raw_win.get("windowDurationMins")
    if mins is None:
        mins = raw_win.get("window_minutes")
    key = fallback_key
    if not isinstance(mins, bool) and isinstance(mins, (int, float)):
        if mins <= 360:
            key = "five_hour"
        elif mins >= 1440:
            key = "seven_day"
    candidate = {"used_percentage": used, "resets_at": resets}
    if observed_at:
        candidate["observed_at"] = observed_at
    win = valid_window(candidate, now)
    return (key, win) if key and win else (None, None)


def parse_codex_rate_limits(rate_obj, now, observed_at=None):
    if not isinstance(rate_obj, dict):
        return False, {}, "", ""
    primary_raw = rate_obj.get("primary")
    secondary_raw = rate_obj.get("secondary")
    expected_count = (1 if isinstance(primary_raw, dict) else 0) + (1 if isinstance(secondary_raw, dict) else 0)
    windows = {}
    for raw_win, fallback in ((primary_raw, "five_hour"), (secondary_raw, "seven_day")):
        key, win = classify_codex_window(raw_win, fallback, now, observed_at=observed_at)
        if key and win:
            windows[key] = win
    plan_raw = rate_obj.get("planType") or rate_obj.get("plan_type") or ""
    plan = plan_raw.strip() if isinstance(plan_raw, str) else ""
    single = ""
    if expected_count == 1 and len(windows) == 1 and secondary_raw is None:
        single = next(iter(windows))
    return expected_count > 0, windows, single, plan


def scan_codex_passive_rollouts(config_dir, base, identity_subject):
    """Read recent local Codex session JSONL files for passive rate_limits snapshots."""
    sessions_dir = config_dir / "sessions"
    if not ordinary_path(config_dir) or not sessions_dir.is_dir() or not ordinary_path(sessions_dir):
        return
    candidates = []
    try:
        for year_dir in sessions_dir.iterdir():
            if not ordinary_path(year_dir) or not year_dir.is_dir():
                continue
            for month_dir in year_dir.iterdir():
                if not ordinary_path(month_dir) or not month_dir.is_dir():
                    continue
                for day_dir in month_dir.iterdir():
                    if not ordinary_path(day_dir) or not day_dir.is_dir():
                        continue
                    for entry in day_dir.iterdir():
                        if ordinary_path(entry) and entry.is_file() and entry.suffix == ".jsonl":
                            try:
                                candidates.append((entry.stat().st_mtime, entry))
                            except OSError:
                                pass
    except OSError:
        return
    if not candidates:
        return
    candidates.sort(key=lambda item: item[0], reverse=True)
    now = int(time.time())
    best_stamp = 0
    best_rate = None
    for _, path in candidates[:3]:
        try:
            size = path.stat().st_size
            with path.open("rb") as stream:
                if size > (512 << 10):
                    stream.seek(size - (512 << 10))
                    stream.readline()
                tail = stream.read(512 << 10)
        except OSError:
            continue
        for raw_line in tail.splitlines():
            if b"rate_limits" not in raw_line:
                continue
            try:
                row = json.loads(raw_line)
            except ValueError:
                continue
            if not isinstance(row, dict) or row.get("type") != "event_msg":
                continue
            payload = row.get("payload")
            if not isinstance(payload, dict) or payload.get("type") != "token_count":
                continue
            rate = payload.get("rate_limits")
            stamp = parse_iso_timestamp(row.get("timestamp"))
            if isinstance(rate, dict) and stamp and 0 < stamp <= now + 60 and stamp >= best_stamp:
                best_stamp = min(stamp, now)
                best_rate = rate
    if best_rate is not None:
        has_expected, windows, single, plan = parse_codex_rate_limits(best_rate, now, observed_at=best_stamp)
        if has_expected and windows:
            save_quota(base, identity_subject, windows, single_window=single, plan=plan, observed_at=best_stamp)


def invoke_codex_usage(binary, env, cwd, is_default, timeout):
    args = [str(binary)]
    if not is_default:
        args += ["-c", 'cli_auth_credentials_store="file"']
    args += [
        "-c", "features.hooks=false",
        "-c", "features.plugins=false",
        "-c", "features.memories=false",
        "-c", "features.apps=false",
        "app-server",
        "--listen", "stdio://",
    ]
    proc = subprocess.Popen(
        args,
        cwd=str(cwd),
        env=env,
        stdin=subprocess.PIPE,
        stdout=subprocess.PIPE,
        stderr=subprocess.DEVNULL,
        text=True,
    )
    requests = [
        {
            "jsonrpc": "2.0",
            "id": 1,
            "method": "initialize",
            "params": {
                "clientInfo": {"name": "supercage", "version": "0.2.0"},
                "capabilities": {"experimentalApi": True},
            },
        },
        {"jsonrpc": "2.0", "method": "initialized", "params": {}},
        {"jsonrpc": "2.0", "id": 2, "method": "account/read", "params": {"refreshToken": False}},
        {"jsonrpc": "2.0", "id": 3, "method": "account/rateLimits/read", "params": {"excludeResetCreditDetails": True}},
    ]
    account_reply = None
    limits_reply = None
    try:
        for req in requests:
            proc.stdin.write(json.dumps(req) + "\n")
        proc.stdin.flush()
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline and (account_reply is None or limits_reply is None):
            remaining = max(0.05, deadline - time.monotonic())
            if os.name != "nt":
                ready, _, _ = select.select([proc.stdout], [], [], min(0.4, remaining))
                if not ready:
                    if proc.poll() is not None:
                        break
                    continue
            line = proc.stdout.readline()
            if not line:
                break
            if len(line) > (1 << 20):
                raise ValueError("oversized app-server response")
            msg = json.loads(line)
            if not isinstance(msg, dict):
                continue
            if msg.get("id") == 2:
                account_reply = msg
            elif msg.get("id") == 3:
                limits_reply = msg
    finally:
        try:
            proc.stdin.close()
        except OSError:
            pass
        if proc.poll() is None:
            proc.terminate()
            try:
                proc.wait(timeout=1.5)
            except subprocess.TimeoutExpired:
                proc.kill()

    reported_email = ""
    reported_plan = ""
    if isinstance(account_reply, dict) and isinstance(account_reply.get("result"), dict):
        acc = account_reply["result"].get("account")
        if isinstance(acc, dict):
            if acc.get("type") == "apiKey":
                return "not_applicable", {}, "", "", ""
            if isinstance(acc.get("email"), str):
                reported_email = acc["email"].strip().lower()
            if isinstance(acc.get("planType"), str):
                reported_plan = acc["planType"].strip()

    if not isinstance(limits_reply, dict) or "error" in limits_reply:
        raise ValueError("codex rateLimits query failed")
    result = limits_reply.get("result")
    if not isinstance(result, dict):
        raise ValueError("missing codex rateLimits result")
    rate_obj = result.get("rateLimits")
    if not isinstance(rate_obj, dict):
        by_id = result.get("rateLimitsByLimitId")
        if isinstance(by_id, dict) and isinstance(by_id.get("codex"), dict):
            rate_obj = by_id["codex"]
    now = int(time.time())
    has_expected, windows, single, rate_plan = parse_codex_rate_limits(rate_obj, now, observed_at=now)
    final_plan = rate_plan or reported_plan
    if not has_expected:
        return "unavailable", {}, "", final_plan, reported_email
    expected_count = (1 if isinstance((rate_obj or {}).get("primary"), dict) else 0) + (
        1 if isinstance((rate_obj or {}).get("secondary"), dict) else 0
    )
    if len(windows) == 0:
        return "unavailable", {}, "", final_plan, reported_email
    status = "reported" if len(windows) >= expected_count else "partial"
    return status, windows, single, final_plan, reported_email


def normalize_tui_text(raw_bytes):
    text = raw_bytes.decode("utf-8", "ignore")
    text = re.sub(r"\x1b\[(\d+)C", lambda m: " " * min(40, int(m.group(1))), text)
    text = re.sub(r"\x1b\[(?:\d+;\d+[Hf]|\d+[BEF])", "\n", text)
    text = ANSI_ESCAPE_RE.sub("", text)
    return text.replace("\r", "\n")


def parse_refresh_delta_seconds(text):
    match = re.search(r"Refreshes\s+in\s+((?:\d+\s*[dhms]\s*)+)", text, re.IGNORECASE)
    if not match:
        return None
    total = 0
    found = False
    for amount_str, unit in re.findall(r"(\d+)\s*([dhms])", match.group(1), re.IGNORECASE):
        found = True
        amount = int(amount_str)
        u = unit.lower()
        if u == "d":
            total += amount * 86400
        elif u == "h":
            total += amount * 3600
        elif u == "m":
            total += amount * 60
        elif u == "s":
            total += amount
    return max(60, total) if found else None


def parse_agy_quota_screen(raw_bytes, now):
    """Parse the official `agy` `/quota` TUI Models & Quota panel."""
    text = normalize_tui_text(raw_bytes)
    idx = text.rfind("Models & Quota")
    if idx < 0:
        return "unavailable", {}, [], "", ""
    panel = text[idx:]
    reported_email = ""
    acct_match = re.search(r"Account:\s*([^\s<>\"']+@[^\s<>\"']+)", panel)
    if acct_match:
        cand = acct_match.group(1).strip().lower()
        if EMAIL_RE.fullmatch(cand):
            reported_email = cand

    reported_plan = ""
    tier_matches = re.findall(r"\((Google AI [A-Za-z0-9 +._-]{1,32})\)", text)
    if tier_matches:
        reported_plan = tier_matches[-1].strip()

    header_re = re.compile(r"(?m)^\s*([A-Z][A-Z0-9 &/_-]{2,44}MODELS)\s*$")
    headers = list(header_re.finditer(panel))
    sections = []
    if headers:
        for i, h in enumerate(headers):
            end = headers[i + 1].start() if i + 1 < len(headers) else len(panel)
            sections.append((h.group(1).strip(), panel[h.end():end]))
    else:
        sections.append(("GEMINI MODELS", panel))

    limit_re = re.compile(r"(Weekly|Five[\s-]+Hour)\s+Limit\s+Remaining", re.IGNORECASE)
    groups = []
    for raw_title, body in sections:
        upper = raw_title.upper()
        if "GEMINI" in upper:
            gid, glabel = "gemini", "Gemini"
        elif "CLAUDE" in upper or "GPT" in upper:
            gid, glabel = "partner", "Claude / GPT"
        else:
            gid = re.sub(r"[^a-z0-9]+", "_", upper.lower()).strip("_")[:24] or "models"
            glabel = raw_title.title()[:32]

        g_windows = {}
        matches = list(limit_re.finditer(body))
        for j, lm in enumerate(matches):
            seg_end = matches[j + 1].start() if j + 1 < len(matches) else min(len(body), lm.end() + 320)
            segment = body[lm.end():seg_end]
            pct_match = re.search(r"(\d{1,3}(?:\.\d+)?)\s*%", segment)
            if not pct_match:
                continue
            remaining_pct = float(pct_match.group(1))
            if not 0.0 <= remaining_pct <= 100.0:
                continue
            used_pct = round(max(0.0, min(100.0, 100.0 - remaining_pct)), 1)
            delta = parse_refresh_delta_seconds(segment)
            if delta is not None:
                resets_at = now + delta
            elif re.search(r"Quota\s+available", segment, re.IGNORECASE):
                resets_at = 0
            else:
                resets_at = 0
            kind = "seven_day" if lm.group(1).lower().startswith("week") else "five_hour"
            win = valid_window({"used_percentage": used_pct, "resets_at": resets_at, "observed_at": now}, now)
            if win:
                g_windows[kind] = win
        if g_windows:
            groups.append({"id": gid, "label": glabel, **g_windows})

    if not groups:
        return "unavailable", {}, [], reported_plan, reported_email

    primary_group = next((g for g in groups if g["id"] == "gemini"), groups[0])
    windows = {k: primary_group[k] for k in ("five_hour", "seven_day") if k in primary_group}
    status = "reported" if len(windows) == 2 else ("partial" if windows else "unavailable")
    return status, windows, groups, reported_plan, reported_email


def invoke_agy_usage(binary, env, cwd, timeout):
    if pty is None or termios is None or fcntl is None:
        raise ValueError("PTY unavailable on this platform")
    master_fd, slave_fd = pty.openpty()
    fcntl.ioctl(slave_fd, termios.TIOCSWINSZ, struct.pack("HHHH", 60, 140, 0, 0))
    inline_settings = json.dumps({
        "trustedWorkspaces": [str(cwd)],
        "toolPermission": "always-proceed",
        "artifactReviewPolicy": "always-proceed",
        "allowNonWorkspaceAccess": True,
    })
    proc = None
    buf = bytearray()
    try:
        proc = subprocess.Popen(
            [str(binary), "--settings", inline_settings],
            stdin=slave_fd,
            stdout=slave_fd,
            stderr=slave_fd,
            cwd=str(cwd),
            env=env,
            close_fds=True,
        )
        os.close(slave_fd)
        slave_fd = -1

        start = time.monotonic()
        deadline = start + timeout
        stage = "wait_prompt"
        sent_theme_confirm = False
        quota_sent_at = 0.0
        last_chunk_at = start

        while time.monotonic() < deadline:
            ready, _, _ = select.select([master_fd], [], [], 0.18)
            if ready:
                try:
                    chunk = os.read(master_fd, 16384)
                except OSError as exc:
                    if exc.errno == errno.EIO:
                        break
                    raise
                if not chunk:
                    break
                buf.extend(chunk)
                if len(buf) > (2 << 20):
                    buf = buf[-(1 << 20):]
                last_chunk_at = time.monotonic()

            text = normalize_tui_text(bytes(buf[-65536:]))
            elapsed = time.monotonic() - start

            if stage == "wait_prompt":
                if "? for shortcuts" in text:
                    os.write(master_fd, b"/quota")
                    time.sleep(0.22)
                    os.write(master_fd, b"\r")
                    stage = "wait_panel"
                    quota_sent_at = time.monotonic()
                elif not sent_theme_confirm and "[Next]" in text and "Confirm" in text and elapsed > 1.2:
                    os.write(master_fd, b"\r")
                    sent_theme_confirm = True
                elif "Select login method" in text and "Signing in" not in text[-600:] and elapsed > 4.5:
                    break
            elif stage == "wait_panel":
                panel_pos = text.rfind("Models & Quota")
                if panel_pos >= 0:
                    panel_tail = text[panel_pos:]
                    has_footer = "esc Close" in panel_tail
                    has_metrics = "%" in panel_tail and ("Refreshes in" in panel_tail or "Quota available" in panel_tail)
                    quiet = (time.monotonic() - last_chunk_at) > 0.65
                    if (has_footer and has_metrics) or (has_metrics and quiet and (time.monotonic() - quota_sent_at) > 1.2):
                        os.write(master_fd, b"\x1b")
                        time.sleep(0.1)
                        os.write(master_fd, b"\x04\x04")
                        break

            if proc.poll() is not None:
                break
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
        if proc is not None and proc.poll() is None:
            proc.terminate()
            try:
                proc.wait(timeout=1.5)
            except subprocess.TimeoutExpired:
                proc.kill()

    return parse_agy_quota_screen(bytes(buf), int(time.time()))


def identity_for(provider_id, config_dir, env, is_default=False):
    if provider_id == "codex":
        return codex_identity(config_dir, env, is_default=is_default)
    return antigravity_identity(config_dir, env, is_default=is_default)


def environment_for(provider_id, base):
    if provider_id == "codex":
        return codex_environment(base)
    return antigravity_environment(base)


def binary_for(provider_id, env):
    override = os.environ.get("CAGE_PROVIDER_CLI_BINARY")
    if override:
        return override
    name = "codex" if provider_id == "codex" else "agy"
    return shutil.which(name, path=env.get("PATH", os.defpath))


def strip_internal_fields(summary):
    out = dict(summary)
    out.pop("api_key_mode", None)
    return out


def public_quota_payload(stored):
    if not isinstance(stored, dict):
        return None
    out = {"observed_at": stored["observed_at"]}
    for key in ("five_hour", "seven_day", "groups", "single_window"):
        if key in stored:
            out[key] = stored[key]
    return out


def antigravity_desktop_emails():
    home = user_home()
    directories = [home / ".config/Antigravity"]
    profiles = home / ".local/share/desktop-profiles"
    if profiles.is_dir() and ordinary_path(profiles):
        for p in profiles.iterdir():
            if not ACCOUNT_ID_RE.fullmatch(p.name) or not ordinary_path(p):
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
            if not ordinary_path(directory) or not ordinary_path(path) or not path.is_file():
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


def snapshot_account(provider_id, account_id):
    base = account_base(provider_id, account_id)
    config_dir = config_dir_for(provider_id, base)
    env = environment_for(provider_id, base)
    summary = identity_for(provider_id, config_dir, env, is_default=(account_id == DEFAULT))
    subj = summary.get("subject") or ""
    if summary.get("logged_in") and subj:
        if provider_id == "codex":
            scan_codex_passive_rollouts(config_dir, base, subj)
        cached = read_quota(base)
        if cached and cached.get("identity") == subj:
            summary["quota"] = public_quota_payload(cached)
            if cached.get("plan") and summary.get("plan") in ("", "antigravity", "codex"):
                summary["plan"] = cached["plan"]
    out = strip_internal_fields(summary)
    if account_id == DEFAULT and provider_id == "antigravity":
        out["desktop_emails"] = antigravity_desktop_emails()
    return out


def query_usage(provider_id, account_id, expected_subject):
    if not SUBJECT_RE.fullmatch(expected_subject or ""):
        raise ValueError("invalid expected subject")
    base = account_base(provider_id, account_id)
    config_dir = config_dir_for(provider_id, base)
    env = environment_for(provider_id, base)
    before = identity_for(provider_id, config_dir, env, is_default=(account_id == DEFAULT))
    if not before.get("logged_in") or before.get("subject") != expected_subject:
        return {"logged_in": False, "usage_status": "identity_changed", "usage_checked_at": int(time.time())}

    if provider_id == "codex":
        scan_codex_passive_rollouts(config_dir, base, expected_subject)

    if before.get("api_key_mode"):
        out = strip_internal_fields(before)
        out["usage_status"] = "not_applicable"
        out["usage_checked_at"] = int(time.time())
        return out

    binary = binary_for(provider_id, env)
    if not binary:
        out = strip_internal_fields(before)
        out["usage_status"] = "unavailable"
        out["usage_checked_at"] = int(time.time())
        return out

    deadline = time.monotonic() + 15.0
    status = "error"
    windows = {}
    groups = []
    single_window = ""
    reported_plan = ""
    reported_email = ""

    with tempfile.TemporaryDirectory(prefix=f"cage-{provider_id}-usage-") as cwd:
        try:
            timeout = max(2.0, deadline - time.monotonic() - 1.5)
            if provider_id == "codex":
                status, windows, single_window, reported_plan, reported_email = invoke_codex_usage(
                    binary, env, cwd, account_id == DEFAULT, min(11.0, timeout)
                )
            else:
                status, windows, groups, reported_plan, reported_email = invoke_agy_usage(
                    binary, env, cwd, min(13.5, timeout)
                )
        except (OSError, ValueError, TypeError, subprocess.SubprocessError):
            status = "error"

    after = identity_for(provider_id, config_dir, env, is_default=(account_id == DEFAULT))
    if not after.get("logged_in") or after.get("subject") != expected_subject:
        return {"logged_in": False, "usage_status": "identity_changed", "usage_checked_at": int(time.time())}
    if reported_email and reported_email != after.get("email", "").lower():
        return {"logged_in": False, "usage_status": "identity_changed", "usage_checked_at": int(time.time())}

    if windows or groups:
        save_quota(
            base,
            expected_subject,
            windows,
            groups=groups,
            single_window=single_window,
            plan=reported_plan,
            observed_at=int(time.time()),
            deadline=deadline,
        )

    summary = strip_internal_fields(after)
    if reported_plan:
        summary["plan"] = reported_plan
    cached = read_quota(base)
    if cached and cached.get("identity") == expected_subject:
        summary["quota"] = public_quota_payload(cached)
        if not reported_plan and cached.get("plan") and summary.get("plan") in ("", "antigravity", "codex"):
            summary["plan"] = cached["plan"]
        if status == "unavailable":
            status = "cached"
    summary["usage_status"] = status
    summary["usage_checked_at"] = int(time.time())
    return summary


def batch_quotas(provider_id, account_ids):
    result = {}
    for aid in account_ids:
        if aid != DEFAULT and not ACCOUNT_ID_RE.fullmatch(aid or ""):
            raise ValueError("invalid account ID")
        base = account_base(provider_id, aid)
        config_dir = config_dir_for(provider_id, base)
        env = environment_for(provider_id, base)
        ident = identity_for(provider_id, config_dir, env, is_default=(aid == DEFAULT))
        subj = ident.get("subject") or ""
        if provider_id == "codex" and ident.get("logged_in") and subj:
            scan_codex_passive_rollouts(config_dir, base, subj)
        cached = read_quota(base)
        if cached:
            entry = public_quota_payload(cached)
            entry["identity"] = cached["identity"]
            result[aid] = entry
    return result


def main(argv):
    os.umask(0o077)
    if len(argv) < 2:
        raise ValueError("missing provider and mode")
    provider_id, mode = argv[0], argv[1]
    if provider_id not in ("codex", "antigravity"):
        raise ValueError("unsupported provider")
    if mode == "snapshot":
        if len(argv) != 3:
            raise ValueError("invalid snapshot arguments")
        print(json.dumps(snapshot_account(provider_id, argv[2]), ensure_ascii=False))
        return 0
    if mode == "usage":
        if len(argv) != 4:
            raise ValueError("invalid usage arguments")
        print(json.dumps(query_usage(provider_id, argv[2], argv[3]), ensure_ascii=False))
        return 0
    if mode == "quotas":
        print(json.dumps(batch_quotas(provider_id, argv[2:]), ensure_ascii=False))
        return 0
    raise ValueError("unknown mode")


if __name__ == "__main__":
    try:
        sys.exit(main(sys.argv[1:]))
    except Exception:
        print('{"error":"provider_usage_unavailable"}')
        sys.exit(1)
