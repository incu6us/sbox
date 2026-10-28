#!/usr/bin/env python3
"""Writes sbox's Claude Code settings into the runner's home, in the guest.

Usage: sbox-claude.py <want.json> <home>    (run as the runner)

want.json is {"settings": {...}, "mcpServers": {...}} from the config.
Each key under "settings" replaces that key in ~/.claude/settings.json
(sbox owns them: permissions, and the bypass prompt when permissions are
skipped); each server under "mcpServers" replaces the one of that name in
~/.claude.json. Everything else in both files, Claude Code's own state,
is kept. A file that isn't a JSON object is started over.
"""
import json
import os
import sys
import tempfile


def merge(settings, claude, want):
    settings.update(want.get("settings", {}))
    servers = claude.get("mcpServers")
    if not isinstance(servers, dict):
        servers = claude["mcpServers"] = {}
    servers.update(want.get("mcpServers", {}))


def load(path):
    try:
        with open(path) as f:
            data = json.load(f)
        return data if isinstance(data, dict) else {}
    except (OSError, ValueError):
        return {}


def save(path, data):
    os.makedirs(os.path.dirname(path), exist_ok=True)
    fd, tmp = tempfile.mkstemp(dir=os.path.dirname(path))
    with os.fdopen(fd, "w") as f:
        json.dump(data, f, indent=2)
        f.write("\n")
    os.replace(tmp, path)


def apply(want_path, home):
    with open(want_path) as f:
        want = json.load(f)
    settings_path = os.path.join(home, ".claude", "settings.json")
    claude_path = os.path.join(home, ".claude.json")
    settings, claude = load(settings_path), load(claude_path)
    merge(settings, claude, want)
    save(settings_path, settings)
    save(claude_path, claude)


def selftest():
    settings = {"theme": "dark", "permissions": {"allow": ["Bash(old)"]}}
    claude = {"numStartups": 3, "mcpServers": {"mine": {"command": "a"}, "pg": {"command": "old"}}}
    want = {
        "settings": {"permissions": {"deny": ["WebFetch"], "defaultMode": "bypassPermissions"}, "skipDangerousModePermissionPrompt": True},
        "mcpServers": {"pg": {"command": "npx", "args": ["-y", "pg"]}, "docs": {"type": "http", "url": "https://mcp.example.com"}},
    }
    merge(settings, claude, want)
    assert settings == {"theme": "dark", "permissions": {"deny": ["WebFetch"], "defaultMode": "bypassPermissions"}, "skipDangerousModePermissionPrompt": True}, settings
    assert claude["numStartups"] == 3 and claude["mcpServers"]["mine"] == {"command": "a"}, claude
    assert claude["mcpServers"]["pg"] == {"command": "npx", "args": ["-y", "pg"]} and "docs" in claude["mcpServers"], claude

    home = tempfile.mkdtemp()
    with open(os.path.join(home, ".claude.json"), "w") as f:
        f.write("[not an object]")
    want_path = os.path.join(home, "want.json")
    with open(want_path, "w") as f:
        json.dump(want, f)
    apply(want_path, home)
    assert load(os.path.join(home, ".claude.json"))["mcpServers"]["docs"]["type"] == "http"
    assert load(os.path.join(home, ".claude", "settings.json"))["skipDangerousModePermissionPrompt"] is True
    print("ok")


if __name__ == "__main__":
    if sys.argv[1:] == ["--selftest"]:
        selftest()
    elif len(sys.argv) == 3:
        apply(sys.argv[1], sys.argv[2])
    else:
        sys.exit("usage: sbox-claude.py <want.json> <home> | --selftest")
