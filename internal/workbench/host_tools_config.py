# Adds one MCP server to every supported AI client of this user:
# - Claude Code reads it over HTTP (.claude.json)
# - Claude Desktop, Antigravity (CLI & Desktop), and Codex (CLI & Desktop)
#   start a private local stdio relay program instead.
# ENTRY and RELAY (JSON strings) are defined by the lines placed before this
# script; the server's name is the first argument and "add" or "remove" the
# second.
import json
import os
import re
import sys
import tempfile
from pathlib import Path


def profile_files(home):
    files = [home / ".claude.json"]
    accounts = home / ".local/share/cage/claude-accounts"
    files += [d / ".claude.json" for d in sorted(accounts.glob("*/config")) if d.is_dir()]
    desktops = home / ".local/share/desktop-profiles"
    files += [d / ".claude.json" for d in sorted(desktops.glob("*/code")) if d.is_dir()]
    return files


def desktop_config_dirs(home):
    dirs = []
    default = home / ".config/Claude"
    if default.is_dir():
        dirs.append(default)
    desktops = home / ".local/share/desktop-profiles"
    for d in sorted(desktops.glob("*/data")):
        if not d.is_dir():
            continue
        provider_file = d.parent / "provider"
        try:
            provider = provider_file.read_text(encoding="utf-8").strip()
        except OSError:
            provider = "claude"
        if provider in ("", "claude"):
            dirs.append(d)
    return dirs


def antigravity_mcp_files(home):
    files = []
    default = home / ".gemini"
    if default.is_dir():
        files.append(default / "config/mcp_config.json")
        if (default / "antigravity").is_dir():
            files.append(default / "antigravity/mcp_config.json")
    accounts = home / ".local/share/cage/antigravity-accounts"
    files += [d / "config/mcp_config.json" for d in sorted(accounts.glob("*/config")) if d.is_dir()]
    desktops = home / ".local/share/desktop-profiles"
    for d in sorted(desktops.glob("*/home/.gemini")):
        if d.is_dir():
            files.append(d / "config/mcp_config.json")
            if (d / "antigravity").is_dir():
                files.append(d / "antigravity/mcp_config.json")
    return files


def codex_toml_files(home):
    files = []
    default = home / ".codex"
    if default.is_dir():
        files.append(default / "config.toml")
    accounts = home / ".local/share/cage/codex-accounts"
    files += [d / "config.toml" for d in sorted(accounts.glob("*/config")) if d.is_dir()]
    desktops = home / ".local/share/desktop-profiles"
    for d in sorted(desktops.glob("*/home/.codex")):
        if d.is_dir():
            files.append(d / "config.toml")
    return files


def write_private(path, text, mode=0o600):
    """Replaces path in one step; returns whether its content changed."""
    try:
        if path.read_text(encoding="utf-8") == text:
            return False
    except (FileNotFoundError, ValueError):
        pass
    path.parent.mkdir(parents=True, exist_ok=True)
    handle, temporary = tempfile.mkstemp(dir=path.parent, prefix="." + path.name + ".")
    try:
        with os.fdopen(handle, "w", encoding="utf-8") as output:
            output.write(text)
        os.chmod(temporary, mode)
        os.replace(temporary, path)
    except BaseException:
        os.unlink(temporary)
        raise
    return True


def add_server(path, name, entry, ours):
    """Adds the entry to a JSON client's configuration; returns whether it wrote."""
    try:
        data = json.loads(path.read_text(encoding="utf-8"))
    except FileNotFoundError:
        data = {}
    except ValueError:
        return False
    if not isinstance(data, dict):
        return False
    servers = data.get("mcpServers")
    if not isinstance(servers, dict):
        servers = {}
    current = servers.get(name)
    if current == entry or (current is not None and not ours(current)):
        return False
    servers[name] = entry
    data["mcpServers"] = servers
    mode = path.stat().st_mode & 0o777 if path.exists() else 0o600
    return write_private(path, json.dumps(data, indent=2), mode)


def remove_server(path, name, ours):
    """Removes the entry written here from a JSON client's configuration."""
    try:
        data = json.loads(path.read_text(encoding="utf-8"))
    except (FileNotFoundError, ValueError):
        return False
    servers = data.get("mcpServers") if isinstance(data, dict) else None
    if not isinstance(servers, dict) or name not in servers or not ours(servers[name]):
        return False
    del servers[name]
    if not servers:
        del data["mcpServers"]
    return write_private(path, json.dumps(data, indent=2), path.stat().st_mode & 0o777)


def _toml_header(name):
    if re.fullmatch(r"[A-Za-z0-9_-]+", name):
        return f"[mcp_servers.{name}]"
    return f"[mcp_servers.{json.dumps(name)}]"


def _toml_block(name, local):
    header = _toml_header(name)
    return (
        f"# cage-managed-mcp-start:{name}\n"
        f"{header}\n"
        f"command = {json.dumps(local['command'])}\n"
        f"args = {json.dumps(local['args'])}\n"
        f"# cage-managed-mcp-end:{name}\n"
    )


def add_toml_server(path, name, local):
    try:
        existing = path.read_text(encoding="utf-8")
    except FileNotFoundError:
        existing = ""
    except OSError:
        return False
    start_marker = f"# cage-managed-mcp-start:{name}\n"
    end_marker = f"# cage-managed-mcp-end:{name}\n"
    block = _toml_block(name, local)
    header = _toml_header(name)
    if start_marker in existing and end_marker in existing:
        before, rest = existing.split(start_marker, 1)
        _, after = rest.split(end_marker, 1)
        updated = before + block + after
    else:
        if any(line.strip() == header for line in existing.splitlines()):
            return False
        sep = "" if not existing or existing.endswith("\n\n") else ("\n" if existing.endswith("\n") else "\n\n")
        updated = existing + sep + block
    mode = path.stat().st_mode & 0o777 if path.exists() else 0o600
    return write_private(path, updated, mode)


def remove_toml_server(path, name):
    try:
        existing = path.read_text(encoding="utf-8")
    except (FileNotFoundError, OSError):
        return False
    start_marker = f"# cage-managed-mcp-start:{name}\n"
    end_marker = f"# cage-managed-mcp-end:{name}\n"
    if start_marker not in existing or end_marker not in existing:
        return False
    before, rest = existing.split(start_marker, 1)
    _, after = rest.split(end_marker, 1)
    updated = (before.rstrip("\n") + ("\n\n" if before.strip() and after.strip() else "\n" if (before.strip() or after.strip()) else "") + after.lstrip("\n"))
    return write_private(path, updated, path.stat().st_mode & 0o777)


def relay_files(home, name):
    return home / ".local/share/mcp-relay/relay.py", home / ".config/mcp-relay" / (name + ".json")


def _cleanup_unused_relay(home, name, active_relay):
    if active_relay:
        return
    program, endpoint = relay_files(home, name)
    try:
        endpoint.unlink()
    except FileNotFoundError:
        pass
    remaining = list(endpoint.parent.glob("*.json")) if endpoint.parent.is_dir() else []
    if not remaining:
        try:
            program.unlink()
        except FileNotFoundError:
            pass
    for directory in (endpoint.parent, program.parent):
        try:
            directory.rmdir()
        except OSError:
            pass


def configure(home, name, entry, relay, providers=None):
    enabled = {"claude": True, "antigravity": True, "codex": True}
    if isinstance(providers, dict):
        for key in enabled:
            if key in providers:
                enabled[key] = bool(providers[key])

    ours_http = lambda current: isinstance(current, dict) and current.get("url") == entry["url"]
    if enabled["claude"]:
        for file in profile_files(home):
            add_server(file, name, entry, ours_http)
    else:
        for file in profile_files(home):
            remove_server(file, name, ours_http)

    program, endpoint = relay_files(home, name)
    local = {"command": "/usr/bin/python3", "args": [str(program), str(endpoint)]}
    ours_relay = lambda current: isinstance(current, dict) and current.get("args") == local["args"]

    desktops = desktop_config_dirs(home)
    agy_files = antigravity_mcp_files(home)
    codex_files = codex_toml_files(home)

    need_relay = (
        (enabled["claude"] and bool(desktops))
        or (enabled["antigravity"] and bool(agy_files))
        or (enabled["codex"] and bool(codex_files))
    )
    if need_relay:
        write_private(program, relay)
        write_private(endpoint, json.dumps({"url": entry["url"], "headers": entry.get("headers", {})}))

    if enabled["claude"]:
        for desktop in desktops:
            add_server(desktop / "claude_desktop_config.json", name, local, ours_relay)
    else:
        for desktop in desktops:
            remove_server(desktop / "claude_desktop_config.json", name, ours_relay)

    if enabled["antigravity"]:
        for file in agy_files:
            add_server(file, name, local, ours_relay)
    else:
        for file in agy_files:
            remove_server(file, name, ours_relay)

    if enabled["codex"]:
        for file in codex_files:
            add_toml_server(file, name, local)
    else:
        for file in codex_files:
            remove_toml_server(file, name)

    _cleanup_unused_relay(home, name, need_relay)


def unconfigure(home, name, entry):
    """Takes back what configure wrote, leaving the user's own servers."""
    for file in profile_files(home):
        remove_server(file, name, lambda current: isinstance(current, dict) and current.get("url") == entry["url"])
    program, endpoint = relay_files(home, name)
    args = [str(program), str(endpoint)]
    ours_relay = lambda current: isinstance(current, dict) and current.get("args") == args
    for desktop in desktop_config_dirs(home):
        remove_server(desktop / "claude_desktop_config.json", name, ours_relay)
    for file in antigravity_mcp_files(home):
        remove_server(file, name, ours_relay)
    for file in codex_toml_files(home):
        remove_toml_server(file, name)
    _cleanup_unused_relay(home, name, False)


if __name__ == "__main__":
    if sys.argv[2] == "remove":
        unconfigure(Path.home(), sys.argv[1], json.loads(ENTRY))  # noqa: F821
    else:
        raw_providers = globals().get("PROVIDERS")
        parsed_providers = json.loads(raw_providers) if isinstance(raw_providers, str) else None
        configure(Path.home(), sys.argv[1], json.loads(ENTRY), RELAY, parsed_providers)  # noqa: F821
