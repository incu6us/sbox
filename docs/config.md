# Config file: mounts, egress, secrets (new)

## Goal

One YAML file that says what goes into the microVM, what may come back,
and where the network may go:

```yaml
# sbox.yaml
mounts:
  - host: ~/work/myrepo          # copied in; never a live mount
    guest: /home/runner/work
    mode: rw                     # changes come back to -out, not in place
  - host: ~/.gitconfig
    guest: /home/runner/.gitconfig   # mode defaults to ro

egress:
  - api.anthropic.com            # port defaults to 443
  - "*.github.com"               # any depth of subdomain, not github.com itself
  - github.com
  - github.com:22

secrets:
  GITHUB_TOKEN: env:GITHUB_SBOX_TOKEN   # read from the host environment at run time
  ANTHROPIC_API_KEY: env:ANTHROPIC_API_KEY

push:
  - github.com/org/repo          # host-side check of the repo's remote, and optional host-side push

agent:
  model: claude-haiku-5-5
  max_turns: 15
  timeout: 5m
```

## One surface

The config file is the only place behaviour is set. Flags exist for what
changes per invocation and nothing else:

| Flag | Why it is a flag |
|---|---|
| `<dir>` or URL | the target |
| `-config <path>` | which file, when not the defaults |
| `-fresh` | discard this project's state, this once |
| `-v` | see VM and kernel output, this once |
| `-json` | machine output for this call |
| `-command <cmd>` | run something other than the configured command, this once |
| `-sync`, `-patch`, `-apply`, `-push` | actions on a session, not settings |

Every other knob in this document and in `sbox scan` (timeouts, model,
prompt, prefetch, egress, trace) is a config key only. There is no
`-timeout` flag once the loader lands; the scan flags that exist today are
removed in config commit 2, since they have no users outside this repo.
A flag and a key never carry the same setting, so there is no precedence
between them to explain.

**Layering.** Two files, fixed names, no includes:

1. `~/.config/sbox/config.yaml`: the user's defaults (model, credentials
   as `env:` references, registries, egress always wanted).
2. `./sbox.yaml` in the project root, dev mode only: the project's needs
   (ports, egress, mounts, MCP servers, command). Lists append to the user
   file's lists, scalars replace them.

`sbox scan` reads only the first: the target is untrusted, and this file
grants permissions. The project file in dev mode is yours, which is the
difference.

**Seeing the result.** `sbox config` prints the effective configuration as
YAML with a comment per key naming where it came from (`# user`, `#
project`, `# default`). `sbox init` writes a starter `sbox.yaml` with
every key present and commented, so the file documents itself and nobody
needs the schema in their head. Both are in config commit 2.

A file inside a scan target is never read; see above.

## Format decision

YAML, parsed with `gopkg.in/yaml.v3`. It is the first dependency; the
"standard library only" line in `MILESTONE.md` and `README.md` becomes
"standard library, plus `yaml.v3` for the config file". Strict decoding
(`KnownFields(true)`), so a misspelt key is an error, not a silent default.
Durations and sizes use Go's `time.ParseDuration` form.

## Mounts

There are no live mounts. The Lima VM has no host mounts by design and the
microVM gets one read-only drive built from a tar. A mount is a copy in,
and for `rw`, a copy back.

- **`ro`** (default): the host path is packed into the run tar under
  `mounts/<n>/` and the guest init copies it to `guest`, owned by the
  runner. Files and directories both work; symlinks are kept as links. The
  existing 256 MiB tar cap covers mounts too.
- **`rw`**: same copy in, plus a second writable drive (`out.ext4`, like the
  prefetch drive) that the guest init bind-mounts over each rw `guest`
  path after the copy. After poweroff `sbox-run` packs that drive into
  `/var/lib/sbox/out/<id>.tar`, and the host fetches it with a second
  `limactl shell` call (`cat` then `rm`), extracts it under the `-out`
  folder (default `./sbox-out/<id>/`), and prints a summary: files added,
  changed, deleted, with a unified diff for text files under 64 KiB.
  Nothing is written into the host path. `-apply` copies the result over
  the host path after the diff is shown and confirmed; it is a separate
  step on purpose. `.git` directories are never copied back (see Push),
  and neither are symlinks whose target leaves the mount.
- **Credential shadowing**: a mount whose `guest` path matches
  `_credentialPaths` or `_agentConfigPaths` in `analyze.go` replaces a fake
  credential with a real one. The report lists those mounts in the header
  and the canary rules are marked off for them. The scan still runs.
- Entry point discovery (`findEntryPoints`, prefetch planning) looks only at
  the target, never at mounts.

## Egress

Host patterns, matched in `sbox-proxy.py: parse_allowlist`:

| Pattern | Matches |
|---|---|
| `host` | that host, port 443 |
| `host:22` | that host, that port |
| `host:*` | that host, any port |
| `*.host` | any subdomain at any depth; not `host` itself |

Nothing else: no path patterns, no regular expressions. The proxy is a
CONNECT tunnel without TLS interception, so a URL path is invisible to it;
a pattern like `github.com/org/*` would be a promise sbox cannot keep.
Repository-level control comes from the credential (a fine-grained token
scoped to one repo, or a deploy key) and from `push` below. The self-test
in the proxy gets one case per row.

The `EGRESS` log and its per-entry attribution come from the agent plan;
this file only widens what the allowlist can say.

## Secrets

`secrets` maps a guest variable name to `env:<HOST_VARIABLE>`. Values are
read on the host when the run starts, never stored, and travel as
`sbox/secrets` (0400, root-only on the drive, the same mechanism as the
agent key). The guest init exports them into the job's environment, after
the fake credentials, so a real value replaces the fake one of the same
name. The last 8 characters of every secret join the canary list in
`analyze`, so a real token in a DNS name or on a network tool's command
line is `canary-exfiltration`. The report header names which variables
were real, never their values.

A missing host variable is an error before any VM starts.

## Push

`push` entries are `host/owner/repo`. Before the run, for every rw mount
that is a git repository, the host reads `.git/config` and checks that
each remote's URL is in the list; a remote outside it is an error. This
catches a wrong config, not a hostile skill: inside the VM the skill can
push wherever the token allows, which is why the token should be scoped.

**Never run git on a `.git` that came back from the VM.** A skill can write
`.git/hooks/pre-push`, set `core.sshCommand` or `credential.helper` in
`.git/config`, and any `git` command on the host against that repository
would run it. So the returned result is treated as a working tree only:
`.git` is excluded from the copy back (the diff is computed against the
host's working tree), `-apply` copies files only, and `-push` works on the
host's own repository: it applies the returned changes as a patch
(`git apply` on the diff sbox already printed, then `git commit` with a
message from `-m`), verifies the remote against `push` again, and pushes
with the host's credentials. Hooks in the host's repository are the user's
own and run as usual.

## Extensions

Rule for adding a key: it exists as a flag or a hard-coded constant
today, or a plan in this folder needs it. Nothing speculative. The full
file with everything below:

```yaml
lima:                           # the Lima VM itself; applied by sbox setup, resize stops the VM first
  cpus: 8
  memory_gib: 16
  disk_gib: 60

image:
  packages: [golang-go, rustc]  # added to the guest image from the Ubuntu archive; changes rebuild it

services: [5432, 6379]          # host localhost ports reachable from the guest as host.sbox:<port> (dev mode)
ports: [3000]                   # guest ports forwarded to the host's localhost (dev mode)

vm:
  vcpus: 2                      # sbox-run.sh vm.json, today fixed
  memory_mib: 1024
  scratch_mib: 256              # tmpfs size in guest-init.sh
  max_target_mib: 256           # _maxRunBytes

timeouts:
  setup: 10m                    # each dev.setup / dev.start step
  run: 60s                      # sbox run -timeout
  entry: 20s                    # sbox scan -timeout, per entry point
  prefetch: 3m
  agent: 5m

prefetch:
  enabled: true                 # -no-prefetch
  registries:                   # added to the three defaults in sbox-proxy.py
    - npm.internal.example.com
    - pypi.internal.example.com

entrypoints:
  skip: [mcp]                   # kinds or names never run: hook, mcp, npm, setup.py, script, script-help, agent
  run:                          # declared by hand; supersedes the SKILL.md heuristic for these
    - python3 scripts/convert.py /home/runner/sample/sample.pdf
    - scripts/setup.sh --check
  env:                          # added to every entry point's env -i line
    MYAPP_CONFIG: /home/runner/.config/myapp.toml
  samples: ./fixtures           # replaces the built-in sample files for placeholder fills

plant:                          # extra fake credentials, canary appended to each value
  ~/.config/myapp/token: "myapp_"
  ~/.config/hub/hub.yml: "oauth_token: gho_"

rules:
  expected_hosts:               # lookups and connects to these drop to INFO, shown as "expected"
    - telemetry.vercel.com
  severity:                     # per-rule overrides, in the user's own file only
    agent-dir-write: INFO
  credential_paths:             # appended to the built-in list
    - ~/.config/myapp/
  block_on: CRITICAL            # verdict thresholds: BLOCK at this severity, REVIEW at the next below
  review_on: MEDIUM

report:
  format: text                  # or json; -json wins
  max_listed: 15                # per category
  fail_on: review               # exit non-zero at REVIEW or BLOCK; "block" for a looser CI gate

agent:
  model: claude-haiku-5-5
  max_turns: 15
  prompt: "Use the {name} skill on this project."   # {name} and {description} from SKILL.md
  project:                      # files seeded into /home/runner/project before the run
    README.md: "# demo\n"
    data.json: '{"a": 1}'
```

Stance per group:

| Group | Add when | Why |
|---|---|---|
| `lima`, `image.packages` | config commit 2, applied by `sbox setup` | The Lima VM's 4 CPUs and 6 GiB fit one or two sessions; toolchains beyond Node and Python need the image to change. Both are setup-time, both are stamped so a change is visible. |
| `services`, `ports` | dev mode commit 5 | Host services reachable from the guest, guest ports from the host. |
| `vm`, `timeouts` | config commit 2 | They are constants and flags today; the file is just a place to keep them. A skill that needs 4 GiB or 5 minutes is a real case already seen in prefetch. |
| `prefetch.registries` | config commit 2 | Private registries are the first thing a company user hits. One list appended to `ALLOWED` in the proxy. |
| `entrypoints.run`, `skip`, `env` | with skill-scripts commit 3 | The heuristic will miss commands; a hand-written list is the honest fallback, and `skip` is how to drop a known-noisy MCP server. `run` lines are the user's, not the skill author's, which is why this file is never read from the target. |
| `entrypoints.samples` | with skill-scripts commit 2 | Real inputs beat the built-in samples for a converter skill. |
| `plant` | config commit 4 | Same mechanism as `secrets` in reverse: fake values written by `guest-init.sh`. Lets a user bait a skill that targets a tool sbox doesn't know. Paths are added to `_credentialPaths` automatically. |
| `rules.expected_hosts` | config commit 2 | Milestone 7 found exactly this case (vercel telemetry): a known destination that should be visible but not REVIEW. Shown in the report as expected, never hidden. |
| `rules.severity`, `block_on`, `review_on`, `credential_paths` | config commit 2 | Calibration knobs. Overrides are listed in the report header so a PASS with a downgraded rule is visibly a PASS with a downgraded rule. |
| `report.fail_on`, `format`, `max_listed` | config commit 2 | CI needs a one-line exit policy; the rest are flags today. |
| `agent.prompt`, `project` | agent commit 1 reads them if present | The default prompt and sample files are guesses; a user who knows the skill writes better ones. |

Not added, and why:

- `extends:` or includes between files: one file per user, one per project
  at most; merge rules are a bug farm. Revisit at the second request.
- `${VAR}` interpolation in arbitrary values: `env:` prefixes in `secrets`
  cover the only case where a host value belongs in the file.
- `targets:` lists for batch scans: a shell loop over `sbox scan -json`
  does it; `TestInstalled` in the probe suite is the in-repo version.
- Per-target profiles keyed by path glob: same as `extends`.
- Pinned versions (`claude`, Node, uv) in the file: pins stay in `setup.go`
  with their checksums; a config value without a checksum is a downgrade.
- Anything that turns off the canary, the nonce, the trace, or egress
  logging: those are what the tool is.

Every override that weakens a verdict (`severity`, `expected_hosts`,
`block_on`, `skip`) is echoed in the report header and in JSON under
`config.overrides`, so a report is readable without the file next to it.

## Code touchpoints

| Area | Today | Change |
|---|---|---|
| Config struct | none | `config.go`: `type config struct { Mounts []mount; Egress []string; Secrets map[string]string; Push []string; Agent agentConfig }`, `loadConfig(path)`, `applyFlags(cfg, flags)` |
| CLI | `runMain`, `scanMain` parse flags, build `guestJob` | both take `-config`, `-out`, `-apply`, `-push`; `guestJob` gains `mounts []mount`, `secrets map[string]string`, `out bool` |
| Tar | `run.go: writeRunTar` packs `skill/` and `sbox/*` | adds `mounts/<n>/...` per mount, `sbox/mounts` (one line per mount: `n guest mode`, 0644), `sbox/secrets` (`NAME=value` lines, 0400) |
| VM side | `sbox-run.sh` builds `drive.ext4`, optional `deps.ext4` | adds `out.ext4` (sparse, 1 GiB) when `sbox/mounts` has an rw line, attaches it rw to the main microVM, packs it to `/var/lib/sbox/out/<id>.tar` after poweroff, skips it in `cleanup` |
| Guest init | `guest-init.sh` copies `/drive/skill` to `/home/runner/work`, plants fake credentials | after the credentials: copy each `mounts/<n>` to its guest path; for rw, mount `/dev/vdd` on `/out`, copy there, bind-mount back; export `sbox/secrets` into the job's `env` line after the fake values |
| Proxy | `sbox-proxy.py: parse_allowlist` exact `host:port` | patterns from the egress table; `ALLOWED` becomes a list of (host pattern, port or any); `match(host, port)` |
| Analysis | `analyze(r, canary)` with one marker | `analyze(r, markers []string)`; `carries` loops over markers; secrets' tails appended by `scanMain` |
| Report | header `N entry points in <dir>` | adds `mounts: ... (shadows ~/.ssh)` and `secrets: GITHUB_TOKEN` lines; JSON gets `config` with mounts (paths and modes), egress, secret names |
| Fetch back | none | `run.go: fetchOut(ctx, id, dir)` runs `limactl shell sbox sudo sh -c 'cat /var/lib/sbox/out/<id>.tar && rm -f ...'`, extracts with the same safe-extraction rules as the GitHub plan, prints the diff |
| Push | none | `push.go`: parse `.git/config` remotes (stdlib `bufio`, ini-shaped), compare to the list, `git push` via `os/exec` on the host only with `-push` |
| Docs | "standard library only" in two files | updated in commit 2 |

## Commits

- [x] **1. Egress wildcards.** `parse_allowlist` patterns, self-test, README
    table. Independent of the file; `-egress` accepts them too.
- [x] **2. Config loading.** (Done: the loader with dev mode; for `run` and
    `scan`, the user's file plus `-config`, never the target's; `vm`,
    `timeouts.run|entry|prefetch`, `prefetch.enabled|registries`,
    `rules.expected_hosts|severity|credential_paths|block_on|review_on`,
    `report.format|max_listed|fail_on`, overrides in the header and in
    JSON `config.overrides`; the behaviour flags removed. `sbox config`
    names no source per key.) `config.go`: the struct, `yaml.v3`, strict decode,
    path expansion (`~`), precedence with flags, the "never from the
    target" rule. With it, the keys the Extensions table assigns to this
    commit: `vm`, `timeouts`, `prefetch.registries`, `rules`, `report`,
    and the overrides line in the report header. `go.mod` gains the dependency; the two docs change their
    stdlib line. Tests: a full file, an unknown key, flag override.
- [x] **3. Read-only mounts.** Tar packing, guest copy, cap, tests on
    `writeRunTar` output. (Dev mode: copied in at each boot, under
    `/home/runner` outside the project; `sbox run` and `scan` don't take
    them yet.)
- [x] **4. Secrets.** (Dev mode: from the Mac's environment, root-only on the drive, tmpfs in the guest. `run` and `scan`: the same values as `env`'s last assignments, so a real one replaces the fake of its name; the Mac redacts every line from the guest (value, hex, base64) before printing or judging it, and a `[redacted:NAME]` in a lookup or a network tool's arguments is `canary-exfiltration`, which replaces the planned canary tails; the header and JSON `config.secrets` name the real ones.) Drive file, guest export, canary tails, header line. Tests
    on `analyze` with a secret tail.
- [x] **5. Read-write mounts.** (Done for the folder `sbox run` is given, which is the case that came up: `-apply` and `-patch <file>`. The guest tars its work folder with sync's rules onto a raw drive, its byte count in the first block, so no filesystem the guest wrote is ever mounted; `sbox-run out` hands it over once; sync's plan, diffs and writes do the rest. `mode: rw` for other mounts is left until asked for. Not for dev mode: `sbox sync` covers it.) `out.ext4`, the bind mounts, the fetch call, the
    diff, `-out`, `-apply` with confirmation.
- [x] **6. Push.** (`sbox run -push -m <message>`: the branch's upstream URL, as written in the Mac's `.git/config`, must be in `push:` before the VM starts and again before pushing; the run's files alone are committed, by your git with your credentials. Not for dev mode: `.git` stays the Mac's and you push from there.) Remote check before the run; `-push` applies the returned diff
    to the host's own repository and pushes from there. `.git` from the VM is
    never used.
- [x] **7.** `MILESTONE.md` entry and README section per item above.

## Verification

Checked on the real VM, 2026-10-28.

- [x] `sbox run -config sbox.yaml . -- git status` on an rw-mounted repo shows
  the repo in the guest; a run that edits a file returns it under `-out`
  with the diff printed; the host file is unchanged until `-apply`.
  (As built: the folder given is the rw one; `-apply` prints sync's diffs
  and writes once confirmed, `-patch <file>` stands for `-out`.
  `TestRunApplyOnVM`, and by hand in a git repository.)
- [x] Egress: `*.github.com` lets `api.github.com:443` through and refuses
  `github.com:443`; `github.com:*` allows 22. (`TestEgressOnVM`;
  `TestEgressAnyPortOnVM`, a CONNECT to github.com:22 through the proxy
  answered with GitHub's SSH banner.)
- [x] A probe hook that echoes `$GITHUB_TOKEN` into a DNS name: BLOCK via the
  secret-tail canary; ponytail PASS unchanged with an empty config. (Via
  redaction instead of tails: `TestScanSecretOnVM`, `CRITICAL sends a
  secret from the config out in a DNS lookup`, the token nowhere in the
  report; ponytail PASS with its one LOW.)
- [x] A `push` entry that doesn't match the mounted repo's remote stops the
  run before the VM starts. (By hand: refused in 0.4 s; `TestPushTarget`.)
- [x] Nothing left on the VM side: the out tar is removed once fetched, also on
  a host-side error (cleanup in `sbox-run` deletes `/var/lib/sbox/out/<id>`
  older than one hour on the next run). (`sbox-run out` removes it as it
  sends it; `TestRunApplyOnVM` checks nothing of an applied run, of one
  whose patch couldn't be written on the Mac, or of a timed-out one is
  left under `out/` or `runs/`. Tars never fetched go after a day.)

## Out of scope

- TLS interception and URL path rules; see the egress section.
- Live or shared mounts (virtiofs, vsock file sharing).
- A config inside the target directory, even for hints; if skill-side hints
  are ever wanted they are a separate, data-only file with no permissions
  in it.
