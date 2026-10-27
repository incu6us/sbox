# Agent entry point: Claude Code headless (v2 step 4b)

## Goal

`sbox scan -agent <dir>` runs Claude Code inside the microVM as if a user
had installed the skill and asked for it, under the same strace, DNS
logging and fake credentials as every other entry point. The report gets
one more entry point, `agent claude`, with what the agent actually did
(trace) next to what it said it did (transcript).

## What exists

- `claude` 2.1.296 is in the guest image (`setup.go`, `scripts/provision.sh`),
  doctor checks the version.
- Process attribution (`scan.go: attribute`) and `entryPoint.Harness`: a
  harness program's own events are hidden, what it launches is judged.
- Main-VM egress (`scripts/sbox-run.sh: start_egress`, `guestJob.egress`,
  `sbox/egress` on the drive) with the allowlist proxy; `sbox run -egress`
  exposes it. `sbox scan` does not set egress yet.
- `transcript.go`: parses `--output-format stream-json`, renders the agent
  section. Not wired in.
- `scanScript` runs every entry point with `env -i` and a fixed environment
  including a fake `ANTHROPIC_API_KEY` carrying the canary. `ep.Env` entries
  come after the fixed ones, so they override.

## Design decisions

**Where the skill goes.** The scan copies the directory to
`/home/runner/work`. The agent runs with cwd `/home/runner/project`, a fresh
folder holding a few sample files (`README.md`, `data.json`, `notes.txt`),
and the skill installed where a user would install it:
`/home/runner/.claude/skills/<name>` as a symlink to `/home/runner/work`
(scan.sh does this as root before the agent entry point). The `<name>` comes
from `SKILL.md`'s frontmatter `name:`, falling back to the folder name. Writes
to the project dir are what the skill did to "the user's files" and show in
the trace as `/home/runner/project/...`. Plugins (`plugin.json` with
commands/hooks/agents) are out of scope for the first cut: step 0 checks
whether this `claude` accepts `--plugin-dir`; if it does, a plugin is
installed through it in a follow-up commit.

**The prompt.** Default: `Use the <name> skill on this project.` followed by
the skill's `description:` line, so the agent has the same trigger text a
user's request would carry. `-prompt "..."` overrides it. The prompt goes to
the agent on stdin (`claude -p` reads it there), through the existing
`sbox/in/stdin.N` mechanism.

**The command.**

```
claude -p --output-format stream-json --verbose \
  --max-turns 15 --model <model> --dangerously-skip-permissions
```

`--verbose` is required by stream-json in print mode. Permission skipping is
refused as root; the runner is uid 1000, so it works. `-model` defaults to
the cheapest current model and is a flag, since every scan costs tokens.
The agent's timeout is separate from the per-entry one: `-agent-timeout`,
default 5 minutes. `entryPoint` gets a `Timeout time.Duration` field
(zero means the scan's `-timeout`); `scanScript` and `vmTimeout` use it.

**Credential.** The host reads `ANTHROPIC_API_KEY`, or failing that
`CLAUDE_CODE_OAUTH_TOKEN`, from its own environment. With neither set,
`-agent` fails before any VM starts, with a message that recommends a
dedicated key with a spend limit. The value travels on the run drive as
`sbox/agent-key` (0400, root-only, like `sbox/nonce`), never in `scan.sh`
or `sbox/in/`. `scan.sh` reads it into the environment of the agent entry
point only, overriding the fake key, and nothing else. It never appears in
console output or the report: `redact` from
[hardening.md](hardening.md) item 3 strips it, and its hex and base64
forms, from every line before printing.

The skill's code runs as the same uid as the agent and inherits its
environment, so it can read the real key. That is also true of a skill a
user runs on their machine; the difference here is that the only way out
is the API host. Two mitigations, both cheap: the key's last 8 characters
are treated as a second canary (`analyze` already has `carries`; pass both
markers), so the key in a DNS name or on a network tool's command line is
CRITICAL; and the guest environment sets
`DISABLE_AUTOUPDATER=1 DISABLE_TELEMETRY=1 DISABLE_ERROR_REPORTING=1
CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1`, so the only destination claude
itself contacts is the API.

**Egress.** `-agent` sets `job.egress = ["api.anthropic.com:443"]` and the
agent's `Env` gets `HTTPS_PROXY=http://172.16.0.1:3128` (scan mode doesn't
set it from `guest-init.sh`; the command mode line stays as is). Egress is
per VM, so a hook could also reach the proxy. To attribute the proxy log to
entry points the way the DNS log is attributed, `scan.sh` sends one CONNECT
to `entry-N-$nonce.sbox.invalid:1` before each entry point when egress is on
(`curl -x` is in the image; the proxy logs it as `DENY entry-N-...`).
`collectRuns` splits `EGRESS` markers on that sentinel exactly as it splits
`DNS` ones, and each run gets `egress allowed/refused` lists in the report.
A refused egress from a non-agent entry point is a `connect` finding with
the hostname, which is better than today's anonymous DNS line.

**Harness.** The entry point's `Harness` is `["claude"]`: claude's reads of
`~/.claude.json`, its session files under `~/.claude/`, and its connect to
the proxy are hidden and counted; every process it spawns (the Bash tool's
`sh`, scripts the skill tells it to run) is judged as usual.

**Transcript.** The agent's stdout is the transcript. The root shell in
`scan.sh` redirects it to `/run/sbox/agent.N.jsonl` (root-only directory,
opened by root before the privilege drop, so the agent can't edit it) and
afterwards prints it as `SBOX-$nonce AGENT N <line>` markers, capped at
2000 lines and 64 KiB per line (`cut -c`). A cut line fails to parse and is
skipped, which `parseTranscript` already does; the cap is noted in the
report when hit. `collectRuns` gathers `AGENT` lines per run; `entryRun`
gets `transcript *agentTranscript`; `writeScanReport` calls `writeTranscript`
after the file list; `writeScanJSON` adds `"agent"` to the entry. The
transcript never adds findings that change the verdict; the INFO
cross-check against the trace is [hardening.md](hardening.md) item 5. One exception worth a rule: if the agent's
result subtype is an error (`error_max_turns`, auth failure), the report
says so in the entry's status line, so a scan that never really ran the
skill isn't mistaken for a clean one.

**Cost and determinism.** Agent runs are non-deterministic and cost money.
The report header shows the model and turn count; `-max-turns` is a flag
with a low default. Nothing in the agent path runs unless `-agent` is given.

**Version bumps.** `claude` is pinned by URL and SHA-256 in `setup.go`.
To move: change `_claudeVersion`, the URL and the sum (the sum is the
SHA-256 of the npm tarball; check it against the registry's `integrity`
sha512 as step 1 did), bump `ROOTFS_REVISION` in `provision.sh`, run
`sbox setup`, then commit 0 of this plan again, because the headless flags
are the part most likely to change. The probe suite's `echo` run is the
acceptance test.

## Commits

- [ ] **0. Check the binary.** `sbox run testdata/skills/echo -- claude --help`
    and a headless dry run with a bogus key: confirm the flags above, the
    stream-json shape against `testdata/claude-stream.jsonl`, whether
    `--plugin-dir` exists, and what claude writes under `$HOME` on first
    run (it goes in the harness, but the list belongs in the commit message).
    No code changes.
- [ ] **1. The entry point.** `entrypoints.go`: `agentEntryPoint(dir, model,
    prompt, timeout)` building the command, stdin, `Env`, `Harness`,
    `Timeout`; SKILL.md frontmatter reader (`name:` and `description:` lines,
    no YAML library). `scan.go`: `-agent`, `-model`, `-prompt`,
    `-agent-timeout`, `-max-turns` flags; the install lines in `scanScript`
    (symlink the skill, create the project folder with sample files) emitted
    only for `Kind == "agent"`. Tests: command and stdin for a sample skill;
    frontmatter edge cases (no frontmatter, quoted values).
- [ ] **2. Key and egress.** `run.go`: `guestJob.secret` written as
    `sbox/agent-key` 0400; `scan.sh` reads it for the agent entry only.
    `scanMain`: pick the host variable, refuse to start without one, set
    `job.egress`. The egress sentinel in `scanScript`, `EGRESS` attribution
    in `collectRuns`, egress lines in text and JSON reports. `analyze`: the
    key tail as a second canary. Tests: `collectRuns` with interleaved
    `EGRESS` markers; `analyze` with the key tail in a DNS name.
- [ ] **3. Transcript.** Redirect, `AGENT` markers, `collectRuns`, report and JSON.
    Test: `collectRuns` on a run with AGENT lines yields the parsed
    transcript; a cut line is skipped.
- [ ] **4. Verify on real skills and document.** See below. `MILESTONE.md` v2
    step 4b ✅ with numbers; `README.md` gets an `-agent` section with the
    cost warning and the credential note.

## Verification

Probes live in `testdata/probes/` ([probes.md](probes.md)); the agent ones
run only with `-tags agent` and a key in the environment.

- [ ] `testdata/probes/agent-echo`: a SKILL.md that tells the agent to run
  `scripts/hello.sh` and write `hello.txt` into the project. Expected:
  transcript shows `Skill`, `Bash`; trace shows `sh scripts/hello.sh` and a
  write to `/home/runner/project/hello.txt`; findings none; PASS.
- [ ] `testdata/probes/agent-curl`: a SKILL.md that tells the agent to fetch
  `https://example.com` with curl. Expected: `network-tool` MEDIUM, egress
  `DENY example.com:443` attributed to the agent entry, REVIEW. The
  transcript's `Bash` summary matches the trace.
- [ ] A probe whose script prints `$ANTHROPIC_API_KEY` into a DNS lookup:
  `canary-exfiltration` CRITICAL via the key-tail canary, BLOCK.
- [ ] Ponytail and the calibration set unchanged without `-agent`.
- [ ] No `ANTHROPIC_API_KEY` string anywhere in `-v` output or JSON (grep).
- [ ] VM side clean afterwards: no run folders, namespaces, jails, helpers.
- [ ] Timing and token cost of one echo run go in the milestone entry.

## Out of scope for now

- Plugins (commands, agents, bundled hooks) through `--plugin-dir`: follow-up
  after step 0 says whether the flag exists.
- Multiple prompts or a conversation; one prompt per scan.
- Judging the transcript (for example, flagging a `WebFetch` tool call):
  the trace already shows the connection, and the transcript is the guest's
  word.
