# Run the scripts a SKILL.md tells the agent to run (4b)

## Goal

Most skills have no hooks or MCP servers; their code is in `scripts/` and
runs when the agent follows `SKILL.md`. Today those skills get "no automatic
entry points" and no VM. This item runs those scripts as entry points,
without an agent, an API key or a model call: cheap, deterministic, and the
same trace and findings as everything else. Agent mode (see
[agent-entry-point.md](agent-entry-point.md)) covers what this heuristic
misses; this covers what agent mode makes expensive.

## What exists

- `findEntryPoints` walks config files, `package.json` and `setup.py`.
- `scanScript` runs any `entryPoint.Command` from the skill's root as the
  runner, with stdin from a file; `Env` and `Harness` per entry point.
- The calibration set: 73 installed skill and plugin folders, most without
  entry points today.

## Design

**Finding commands.** `skillScripts(dir)` reads `SKILL.md` (and
`*/SKILL.md` one level down, for plugins that bundle several skills) and
collects candidate commands from:

1. Fenced code blocks tagged `bash`, `sh`, `shell`, `console`, `zsh`, or
   untagged. Each non-empty line, with a leading `$ ` stripped, is a
   candidate; lines ending in `\` are joined.
2. Inline code spans whose text starts with an interpreter or a path into
   the skill (`python3 scripts/x.py`, `scripts/x.sh ...`, `node ...`,
   `uv run ...`, `npx ...`, `bash ...`).

A candidate is kept when its first argument, after an optional interpreter
prefix (`bash`, `sh`, `python`, `python3`, `node`, `uv run`, `npx`), names
a file that exists under the skill directory. `cd` and `pipx install`
lines, `curl | sh`, and anything whose target isn't in the skill are
dropped: the point is to run the skill's own code, not reproduce the
tutorial. Candidates are deduplicated by their argv; at most 10 per skill
(constant, noted in the report when hit).

**Placeholders.** Skills write `scripts/convert.py <input.pdf> output.md`
or `$ARGUMENTS` or `path/to/file`. A fixed substitution table maps them to
sample files the scan ships in `sbox/in/samples/` and copies to
`/home/runner/sample/`: `sample.pdf` (a minimal valid PDF), `sample.json`,
`sample.md`, `sample.txt`, `sample.csv`, `sample.png` (1×1), and
`/home/runner/sample/out/` for outputs. The rules, in order:

- `<...>`, `{...}`, `{{...}}`, `[...]`, `$ARGUMENTS`, `$1`..`$9`: pick the
  sample by any extension mentioned inside the placeholder, else `sample.txt`.
- A path argument that doesn't exist in the skill and has an extension:
  the sample with that extension, else left as is.
- `path/to/...`, `/path/to/...`, `your-...`: as above.

A substituted command is shown in the report as `$ <command>` with a
`(from SKILL.md line N, placeholders filled)` note, so a failure from a bad
fill is easy to read. Fills are a heuristic with a ceiling; the `ponytail:`
comment on the table says so and names the upgrade: an agent run.

**The `--help` fallback.** Every executable or `.sh`/`.py`/`.js` file
directly under `scripts/` (or `bin/`) that no kept candidate references is
run as `<file> --help`, as a separate entry point kind `script-help`. It
exercises imports, top-level code and argument parsing, which is where
install-time payloads live, and almost never does the script's real work.

**Entry point shape.** `Kind: "script"`, `Name: <file>`, `Source:
"SKILL.md:N"`, `Command` the filled line, stdin `/dev/null`, no harness,
the standard per-entry timeout. Prefetch already runs for `requirements*.txt`
and `package.json`, so Python and Node scripts find their dependencies.

## Commits

- [ ] **1. Parser.** `skillscripts.go`: fence and inline-span extraction, the
    keep rule, dedupe and cap. Table tests on snippets, plus a test over
    `testdata/skills/*/SKILL.md` copied from three real installed skills
    (with permission noted in the commit) asserting the exact commands
    found. No placeholder filling yet; candidates with unfilled placeholders
    are kept but marked.
- [ ] **2. Placeholders and samples.** The substitution table, the sample files
    (generated in Go, no binaries checked in except the 1×1 PNG and the
    minimal PDF as short byte literals), `sbox/in/samples/` in the tar and
    the copy in `scan.sh`. Tests: each rule; a command with two placeholders
    of different types.
- [ ] **3. `--help` fallback and wiring.** `findEntryPoints` appends the script
    entry points; the report shows the source line; `-no-scripts` flag to
    turn the heuristic off. JSON gets `"source"` per entry (already there as
    `Source`).
- [ ] **4. Calibrate and document.** Scan the 73 installed folders again. Record:
    how many gained entry points, how many commands ran to exit 0, timeouts,
    and every new finding by hand. Fix fills that fail often. `MILESTONE.md`
    4b's second bullet ✅ with the numbers; README's `sbox scan` section
    lists `script` and `script-help` entry points.

## Verification

- [ ] A test skill whose SKILL.md says `python3 scripts/summarize.py <file.md>`
  runs it on `sample.md`: trace shows `python3 scripts/summarize.py
  /home/runner/sample/sample.md`, exit 0, PASS.
- [ ] A probe whose script reads `~/.aws/credentials` under `--help`:
  `credential-read` HIGH, REVIEW. This is the case the fallback is for.
- [ ] Calibration: no new timeouts beyond scripts that legitimately wait for
  input (those get the `timeout` LOW finding and are listed in the
  milestone entry as the known ceiling).

## Relation to coverage

The inventory in [hardening.md](hardening.md) item 1 lists what never ran.
This plan is what makes that list short; the two share the file walk.

## Out of scope

- Following prose instructions ("run the setup script first, then..."):
  ordering is by appearance in the file.
- Scripts that need a browser, a display or Docker.
- Reading `SKILL.md` for allowed tools or model hints.
