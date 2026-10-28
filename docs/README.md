# Plans

Direction, as of 2026-10-27: sbox is for building projects with an agent
inside a sandbox. `sbox scan` stays as the finished auditing use of the
same machinery; new work goes into dev mode first.

## Active

| # | Item | Plan | Why this order |
|---|---|---|---|
| 1 | Dev mode: terminal, sync, ports, persistence, secrets | [dev-mode.md](dev-mode.md) | The product |
| 1b | `sbox setup` installs Lima itself, with a host preflight | [prerequisites.md](prerequisites.md) | A fresh Mac can't run anything today; small and independent |
| 2 | Config file (mounts, egress wildcards, secrets, push) | [config.md](config.md) | Dev mode's commit 6 is this plan's commits 1, 2 and 4; the rest follows |
| 3 | GitHub URLs as the project source | [github-targets.md](github-targets.md) | `sbox dev github.com/o/r`; stdlib only, independent |
| 4 | Report hardening, items 3 and 4 only (redaction, fingerprint) | [hardening.md](hardening.md) | Redaction before real secrets enter the guest; the rest is scan-only |
| 5 | Firecracker snapshots | [snapshots.md](snapshots.md) | Sessions are long, so boot time matters less than before; measure first still applies |

## Shelved (scan mode)

Kept for when scanning is picked up again; nothing here blocks dev mode.

| Item | Plan |
|---|---|
| Probe suite | [probes.md](probes.md) |
| SKILL.md scripts as entry points | [skill-scripts.md](skill-scripts.md) |
| Claude Code headless agent entry point | [agent-entry-point.md](agent-entry-point.md) (its key transport and egress sentinel are reused by dev mode) |
| Cursor hook payloads and agent profile | [cursor.md](cursor.md) |
| Fake network sink | [fake-services.md](fake-services.md) |
| Hardening items 1, 2, 5 | [hardening.md](hardening.md) |

Conventions shared by every plan:

- Go standard library, plus `gopkg.in/yaml.v3` for the config file (see
  [config.md](config.md)); VM-side helpers are Python 3 standard
  library scripts in `scripts/`, embedded in the binary and installed by
  `sbox setup`.
- Anything the guest reports is untrusted: results travel in nonce-marked
  lines, and the host never runs code that came from the guest.
- Each commit leaves `go test ./...` green and `sbox scan` on the ponytail
  plugin PASS.
- `MILESTONE.md` and `README.md` are updated in the commit that finishes an
  item, in the same voice as the existing entries.

## Status

- [x] 1. Dev mode ([dev-mode.md](dev-mode.md)) (left: the verification steps that need a real token)
  - [x] 1 `sbox-run start|stop|ps|pause|resume`, sessions, API socket
  - [x] 2 SSH into the guest, `sbox dev` terminal
  - [x] 3 writable work drive, persistence, `-fresh`, `sbox clean`
  - [x] 4 `sbox sync`: three-way merge, diff, apply, both directions, `-patch`, the Mac's edits pushed at boot (the live notice left)
  - [x] 5 ports and services (doctor's forwarding check left)
  - [x] 5b egress from the config, wildcards, raw TCP (ssh, databases)
  - [x] 5c reattach and detach
  - [x] 5d toolchains via `image.packages`, a process cap in the guest
  - [x] 5e setup and start hooks, `dev.env`
  - [x] 6 secrets and config keys
  - [x] 7 diagnostics: preflight, BLOCKED lines, summary, `sbox watch`, `sbox log -f|-blocked`, denied tool calls, `-trace`
  - [x] 8 docs and timings
- [x] 1b. Prerequisites ([prerequisites.md](prerequisites.md))
  - [x] 1 host preflight
  - [x] 2 `limactlPath()`
  - [x] 3 pinned Lima download
  - [x] 4 verified on a Mac without Lima
- [x] 2. Config ([config.md](config.md)): every commit; rw mounts as `sbox run -apply`/`-patch` for the folder given and `mode: rw` mounts
- [x] 3. GitHub URLs ([github-targets.md](github-targets.md)): commits 1 to 3, the cache cleanup, `-refetch`, the JSON `target` object
- [x] 4. Hardening ([hardening.md](hardening.md)): items 3 and 4 (redaction for sessions; hostname, but not the work directory name)
- [x] 5. Snapshots ([snapshots.md](snapshots.md)): measured (boot and init 1.8 s of a 2.4 s start); deferred, since a start happens once per dev session
