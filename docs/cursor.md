# Cursor: hook payloads (4b) and an agent profile (v2)

Two items, one file: the payload fix is small and lands first; the agent
profile follows the Claude Code one in
[agent-entry-point.md](agent-entry-point.md) step for step.

## Part 1: Cursor hook payloads

### What exists

`entrypoints.go: hookInput` sends every hook a Claude Code shaped payload
(`session_id`, `transcript_path`, `cwd`, `hook_event_name`) plus one field
per event; `beforeShellExecution` and `afterShellExecution` get `command`
only. `configEntryPoints` finds Cursor hooks because `hooks.json` keys its
`hooks` object by event the same way.

### What to build

A Cursor hook sees a Cursor payload. Detection: the config file is named
`hooks.json` and has a top-level `version` key, or the event name is in
Cursor's set (`beforeShellExecution`, `afterShellExecution`,
`beforeMCPExecution`, `afterMCPExecution`, `afterFileEdit`, `beforeReadFile`,
`beforeSubmitPrompt`, `stop`, `sessionStart`, `sessionEnd`). `entryPoint`
gets an `Agent string` field (`claude` or `cursor`), set in `configEntry`.

`hookInput(agent, event, matcher)` then builds, for Cursor, the common
fields `conversation_id`, `generation_id`, `hook_event_name`,
`workspace_roots: ["/home/runner/work"]`, and per event:

| Event | Fields |
|---|---|
| `beforeShellExecution`, `afterShellExecution` | `command: "ls"`, `cwd` |
| `beforeMCPExecution`, `afterMCPExecution` | `tool_name`, `tool_input`, `url` or `command` |
| `afterFileEdit` | `file_path`, `edits: [{old_string, new_string}]` |
| `beforeReadFile` | `file_path`, `content`, `attachments: []` |
| `beforeSubmitPrompt` | `prompt: "hello"`, `attachments: []` |
| `stop` | `status: "completed"` |

The exact field names are checked against Cursor's hooks documentation at
implementation time (the table is from memory and that is the point of the
item). The commit message quotes the doc version or date used.

Tests: a table test in `entrypoints_test.go` with one Cursor `hooks.json`
per event, asserting the payload keys; the existing Claude cases unchanged.
Verification: the installed `~/.cursor/hooks.json`, if any, through a
`-file` style copy into a temp folder; otherwise a probe hook that echoes
its stdin and exits non-zero on a missing field.

- [ ] One commit: payload builder, detection, table test, verification above.

## Part 2: Cursor agent profile

### Goal

`sbox scan -agent cursor <dir>` does for Cursor's CLI agent what Part 1 of
the Claude plan does for Claude Code: install the skill where Cursor looks
for it, run the agent headless with the user's Cursor credential, egress to
Cursor's API hosts only, trace and transcript in the report.

### Unknowns to settle first (commit 0, no code)

Cursor's agent CLI changes faster than Claude Code's, so the first commit
is a note with answers, found by running things, not by reading:

- [ ] **1. The binary.** Cursor's install script fetches a versioned linux-arm64
    archive. Find the archive URL for one version, its SHA-256, and whether
    it is a single static binary or needs a Node runtime (the image has Node
    24). It gets pinned in `setup.go` like `CLAUDE`.
- [ ] **2. Headless flags.** The equivalents of `-p`, `--output-format
    stream-json`, a model flag, a turn cap, and non-interactive approval of
    commands. The auth variable (`CURSOR_API_KEY` or a login token).
- [ ] **3. Skill location.** Where a user-scope skill lives for the CLI
    (`~/.cursor/skills/<name>/SKILL.md` or project `.cursor/skills/`).
- [ ] **4. API hosts.** Run it once in the VM with egress on and an empty
    allowlist: the proxy log's `DENY` lines are the list of hosts it needs.
    Expect more than one (API, auth, telemetry); allow the minimum that makes
    a run succeed, and set whatever variables turn telemetry off.
- [ ] **5. Transcript shape.** Save one real headless run's output to
    `testdata/cursor-stream.jsonl`.

### Commits

- [ ] **1. Pin and install.** `setup.go` artifact `CURSOR`, `provision.sh`
    extracts it to `/usr/local/bin`, `ROOTFS_REVISION` bump, doctor checks
    the version (`doctor.sh` prints it, `judge` wants it). Image size in the
    commit message.
- [ ] **2. Entry point.** `agentEntryPoint` takes the agent name; a small table
    per agent: binary, headless argv, credential variable(s), egress hosts,
    skill install path, harness program names, quiet-traffic environment.
    The Claude row is the one already built; the Cursor row is new. The
    install lines in `scanScript` read the path from the table.
- [ ] **3. Transcript.** A second decoder in `transcript.go` for Cursor's stream
    format, producing the same `agentTranscript`; `parseTranscript` picks the
    decoder from the agent name. Tested on the saved sample. If the formats
    turn out close enough, one decoder with two field maps; decide after
    seeing the sample, not before.
- [ ] **4. Verify and document.** Same probes as the Claude plan (`echo`,
    `probe-agent`, key tail in a DNS name), plus: nothing but the allowlisted
    hosts in the egress log. `MILESTONE.md` v2 "Then a Cursor profile" ✅,
    `README.md` lists both agents under `-agent`.

### Out of scope

- Cursor rules, commands, and MCP configs as agent-mode inputs; skills only.
- Cursor's IDE hooks running during the agent run: they are already covered
  as entry points of their own.
