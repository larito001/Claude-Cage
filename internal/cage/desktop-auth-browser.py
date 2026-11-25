#!/usr/bin/python3
"""Open external links in the current Linux Desktop's own display and network."""
import json
import os
import re
import shutil
import subprocess
import sys
import tempfile
from pathlib import Path
from urllib.parse import parse_qs, urlsplit

LANG_RE = re.compile(r"^[A-Za-z]{2,3}(?:-[A-Za-z0-9]{2,8})*$")
POSIX_LOCALE_RE = re.compile(r"^([A-Za-z]{2,3})_([A-Za-z]{2,3})(?:\.[^@\s]+)?(?:@[^\s]+)?$")


def valid_language(value):
    value = (value or "").strip()
    return value if LANG_RE.match(value) else ""


def language_from_locale(value):
    match = POSIX_LOCALE_RE.match((value or "").strip())
    if not match:
        return ""
    return valid_language(f"{match.group(1).lower()}-{match.group(2).upper()}")


def read_env_file_language(path):
    try:
        for line in Path(path).read_text(encoding="utf-8", errors="replace").splitlines():
            key, sep, val = line.partition("=")
            if sep and key.strip() == "CAGE_LANGUAGE":
                lang = valid_language(val.strip().strip("'\""))
                if lang:
                    return lang
    except OSError:
        pass
    return ""


def resolve_language(env=None, runtime_dir="", env_file="/etc/claude-cage/environment"):
    env = os.environ if env is None else env
    runtime_dir = runtime_dir or env.get("XDG_RUNTIME_DIR", "")
    if runtime_dir:
        try:
            lang = valid_language(Path(runtime_dir, "browser-language").read_text(encoding="utf-8", errors="replace"))
            if lang:
                return lang
        except OSError:
            pass
    for candidate in (
        valid_language(env.get("CAGE_LANGUAGE", "")),
        read_env_file_language(env_file),
        language_from_locale(env.get("LC_ALL", "")),
        language_from_locale(env.get("LANG", "")),
    ):
        if candidate:
            return candidate
    return "en-US"


def save_language_prefs(profile_dir, lang):
    default_dir = Path(profile_dir) / "Default"
    default_dir.mkdir(parents=True, exist_ok=True)
    prefs_path = default_dir / "Preferences"
    data = {}
    try:
        parsed = json.loads(prefs_path.read_text(encoding="utf-8"))
        if isinstance(parsed, dict):
            data = parsed
    except (OSError, ValueError):
        pass
    intl = data.get("intl")
    if not isinstance(intl, dict):
        intl = {}
        data["intl"] = intl
    intl["accept_languages"] = lang
    intl["selected_languages"] = lang
    tmp_path = prefs_path.with_name("Preferences.tmp")
    tmp_path.write_text(json.dumps(data), encoding="utf-8")
    os.chmod(tmp_path, 0o600)
    os.replace(tmp_path, prefs_path)


def clean_environment(env=None):
    source = os.environ if env is None else env
    return {k: v for k, v in source.items() if not k.upper().startswith("CAGE_")}


def browser_flags(profile_dir, lang):
    return [
        "--user-data-dir=" + str(profile_dir),
        "--no-first-run",
        "--no-default-browser-check",
        "--disable-quic",
        "--force-webrtc-ip-handling-policy=disable_non_proxied_udp",
        "--disable-blink-features=AutomationControlled",
        "--lang=" + lang,
        "--accept-lang=" + lang,
    ]


def _valid_callback(raw, official):
    try:
        callback = urlsplit(raw)
        if callback.username or callback.password:
            return False
        if callback.scheme == "https" and callback.port in (None, 443) and callback.hostname in official:
            return True
        if callback.scheme == "http" and callback.hostname in ("localhost", "127.0.0.1") and callback.port is not None:
            return 1024 <= callback.port <= 65535
    except ValueError:
        pass
    return False


def sign_in(url):
    if url.scheme != "https" or url.username or url.password or url.port not in (None, 443):
        return False
    raw_path = url.path if url.path.startswith("/") else "/" + url.path
    claude = {"claude.ai", "claude.com", "platform.claude.com", "platform.anthropic.com", "console.anthropic.com"}
    google = {"antigravity.google", "antigravity.google.com"}
    codex = {"chatgpt.com", "chat.openai.com", "auth.openai.com", "auth0.openai.com", "platform.openai.com"}
    if url.hostname in claude:
        path = raw_path.removeprefix("/cai/")
        if not path.startswith("/"):
            path = "/" + path
        roots = ("/login", "/auth", "/oauth", "/sso", "/sso-callback", "/magic-link", "/desktop/authorize", "/desktop/callback", "/api/auth")
        return any(path == root or path.startswith(root + "/") for root in roots)
    if url.hostname in google:
        roots = ("/login", "/auth", "/auth-success", "/oauth", "/oauth-callback", "/sso", "/sso-callback", "/desktop/authorize", "/desktop/callback", "/api/auth")
        return any(raw_path == root or raw_path.startswith(root + "/") for root in roots)
    if url.hostname in codex:
        roots = ("/login", "/auth", "/oauth", "/authorize", "/u/login", "/sso", "/sso-callback", "/magic-link", "/codex", "/activate", "/device", "/desktop/authorize", "/desktop/callback", "/api/auth", "/api/accounts")
        return any(raw_path == root or raw_path.startswith(root + "/") for root in roots)
    provider = ((url.hostname == "api.workos.com" and raw_path.startswith("/sso/")) or
                (url.hostname == "accounts.google.com" and (raw_path.startswith("/o/oauth2/") or raw_path == "/AccountChooser" or raw_path.startswith("/AccountChooser/"))) or
                (url.hostname == "login.microsoftonline.com" and "/oauth2/" in raw_path) or
                (url.hostname == "appleid.apple.com" and raw_path.startswith("/auth/")))
    redirects = parse_qs(url.query, keep_blank_values=True).get("redirect_uri", [])
    if not provider or len(redirects) != 1:
        return False
    return _valid_callback(redirects[0], claude | google | codex)


def sign_in_provider(url):
    if not sign_in(url):
        return ""
    claude = {"claude.ai", "claude.com", "platform.claude.com", "platform.anthropic.com", "console.anthropic.com"}
    google = {"antigravity.google", "antigravity.google.com"}
    codex = {"chatgpt.com", "chat.openai.com", "auth.openai.com", "auth0.openai.com", "platform.openai.com"}
    if url.hostname in claude:
        return "claude"
    if url.hostname in google:
        return "antigravity"
    if url.hostname in codex:
        return "codex"
    redirects = parse_qs(url.query, keep_blank_values=True).get("redirect_uri", [])
    if len(redirects) == 1:
        try:
            cb_host = urlsplit(redirects[0]).hostname or ""
            if cb_host in google:
                return "antigravity"
            if cb_host in codex:
                return "codex"
            if cb_host in claude:
                return "claude"
        except ValueError:
            pass
    try:
        desktop_prov = (Path.home().parent / "provider").read_text(encoding="utf-8").strip()
        if desktop_prov in ("claude", "antigravity", "codex"):
            return desktop_prov
    except OSError:
        pass
    if url.hostname == "accounts.google.com":
        return "antigravity"
    return "claude"


def login_reuse_enabled(provider):
    if provider == "antigravity":
        return Path("/etc/claude-cage/login-browser-mode-antigravity").exists()
    if provider == "codex":
        return Path("/etc/claude-cage/login-browser-mode-codex").exists()
    return Path("/etc/claude-cage/login-browser-mode").exists()


def main(address):
    if len(address) > 8192 or any(ord(c) < 32 for c in address):
        return 2
    url = urlsplit(address)
    if url.scheme not in ("http", "https") or not url.hostname:
        return 2
    browser = shutil.which("chromium") or shutil.which("google-chrome-stable")
    if not browser:
        return 1
    temporary = os.environ["XDG_RUNTIME_DIR"]
    lang = resolve_language(os.environ, temporary)
    env = clean_environment(os.environ)
    regular = Path.home() / ".config/chromium"
    provider = sign_in_provider(url)
    if not provider or login_reuse_enabled(provider):
        try:
            save_language_prefs(regular, lang)
        except OSError:
            pass
        os.execve(browser, [browser, *browser_flags(regular, lang), address], env)
    # Each sign-in gets a disposable profile directory in the private runtime
    # directory instead of --incognito, isolating cookies and concurrent flows
    # without setting incognito-detection signals. systemd removes the runtime
    # directory on stop even after an interrupted login.
    with tempfile.TemporaryDirectory(prefix="login-", dir=temporary) as profile:
        try:
            save_language_prefs(profile, lang)
        except OSError:
            pass
        return subprocess.call([browser, *browser_flags(profile, lang), address], env=env)


if __name__ == "__main__":
    try:
        sys.exit(main(sys.argv[1]) if len(sys.argv) == 2 else 2)
    except (OSError, ValueError):
        sys.exit(1)
