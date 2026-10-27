# Report hardening: coverage, unscanned binaries, redaction, fingerprint (new)

Five small, cross-cutting changes. None is a milestone of its own; each is
a commit that makes every report more honest or every run safer.

## 1. Coverage: what never ran

**Problem.** PASS looks the same whether every script ran or none did. The
known limit "only code paths that actually run are observed" is true, but
the report doesn't say what wasn't observed.

**Change.** The host inventories the target before the run: every file
that is executable, has a shebang, or ends in `.sh`, `.py`, `.js`, `.mjs`,
`.ts`, `.rb`, `.pl`, plus every ELF or Mach-O file. After the run, each
inventory entry is matched against the trace's `proc` events (by path
under `/home/runner/work/`) and against the entry points' commands. The
report gains a section:

```
coverage   7 of 12 scripts ran; not run: scripts/deploy.sh, scripts/cleanup.py, bin/helper (ELF x86-64)
```

JSON gets `"coverage": {"ran": [...], "not_run": [...]}`. No finding, no
verdict change; it is context for the reader. `node_modules`, `.git` and
`__pycache__` are skipped, as in `findEntryPoints`.

## 2. Unscanned binaries

**Problem.** The guest is arm64 Linux. A skill that ships an x86-64 Linux
binary, a macOS binary or a Windows executable has code the sandbox never
executes, and the scan passes.

**Change.** Reading 20 bytes per file on the host: ELF (`\x7fELF`, machine
at offset 18: `0xB7` aarch64 runs, anything else doesn't), Mach-O
(`0xfeedfacf`, `0xcafebabe`), PE (`MZ`). Each non-runnable binary is a
finding `unscanned-binary` LOW with its path and kind; a skill with only
such binaries and no other entry point gets REVIEW instead of "no
automatic entry points", because nothing was checked. Lives in the
coverage inventory above.

## 3. Redaction by construction

**Problem.** The agent and config plans carry real secrets into the guest
and verify "no key in the output" by grepping.

**Change.** `runResult` carries the list of secret values; `streamConsole`,
`writeScanReport`, `writeScanJSON` and the `run` summary pass every line
through `redact(s)`, which replaces each secret value, its hex encoding
and its base64 encoding with `[redacted:<NAME>]`. Values shorter than 8
characters are never treated as secrets (they would match noise). A unit
test covers plain, hex and base64. Grepping in verification stays as a
belt, the function is the braces.

## 4. Sandbox fingerprint

**Problem.** A skill can tell it is in sbox from the hostname, `/drive`,
`172.16.0.2`, `/proc/cpuinfo`, and the `runner` user, and stay quiet.

**Change.** The cheap part only: `guest-init.sh` sets a hostname derived
from the nonce (`echo "$name" >/proc/sys/kernel/hostname`, six lowercase
letters, no `sbox`), and the work directory name comes from the target's
own folder name (`/home/runner/<basename>` instead of `/home/runner/work`)
with `_work` in `analyze.go` following it. The `runner` user, the tap
address and `/drive` stay: changing them touches every rule and script for
a gain a determined skill defeats anyway. `MILESTONE.md` Known limits says
exactly which fingerprints remain.

The work-directory change has a cost: `CLAUDE_PROJECT_DIR` and the paths
in `_toolInputs` and `_agentConfigPaths` are built from `_work`. One
constant, several readers; the probe suite catches a missed one.

## 5. Transcript against trace (agent mode)

**Problem.** The transcript is the guest's word. The agent plan keeps it
informational, but comparing it with the trace is cheap and catches two
things: a tool call that did more than it said, and a process that no tool
call explains.

**Change.** After attribution, for an agent entry point: each `Bash` tool
call's command is matched to a `sh -c <command>` process in the trace;
each `proc` event whose parent chain reaches `claude` but matches no tool
call is listed. Two INFO findings: `tool-call-unmatched` (said, not seen:
usually a cap or a parse skip) and `process-unexplained` (seen, not said).
INFO never changes the verdict; the report shows them under the agent
section. The Write and Edit tools don't spawn processes, so they match
against `file` events on their `file_path` instead.

## Commits

- [ ] **1. Inventory and coverage section** (items 1 and 2 share the walk):
    `coverage.go`, the ELF, Mach-O and PE sniff with byte-literal tests,
    report and JSON, `unscanned-binary` rule, REVIEW for binary-only
    targets. README findings table gets the rule.
- [ ] **2. Redaction.** `redact` and its test; wired into the four writers;
    `runResult.secrets`. No behaviour change until a secret exists (the
    agent plan's commit 2 fills the list).
- [ ] **3. Hostname and work directory.** `guest-init.sh`, `_work` readers,
    probe suite green, Known limits updated.
- [ ] **4. Transcript cross-check.** After the agent plan's commit 3:
    matching, the two INFO rules, tests on a transcript and trace pair.

## Verification

- [ ] A skill with three scripts, one referenced by a hook: coverage says 1
    of 3 ran and names the other two.
- [ ] A skill with an x86-64 ELF and nothing else: REVIEW, `unscanned-binary`.
- [ ] A probe hook that prints `$ANTHROPIC_API_KEY` (real, in agent mode)
    base64-encoded: the console line shows `[redacted:ANTHROPIC_API_KEY]`.
- [ ] `hostname` and `pwd` inside a run show the derived name and the
    target's folder name; the ponytail scan is still PASS.
- [ ] Agent probe `echo`: no unmatched calls or unexplained processes; a
    probe SKILL.md that tells the agent to run a script which then forks
    `curl`: `process-unexplained` INFO on the `curl` process, plus the
    usual `network-tool`.
