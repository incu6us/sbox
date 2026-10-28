# Workspaces: what a project may reach outside its VM (new)

## Goal

A workspace is a named set of permissions in your own config file: which
Mac files may be copied into a guest, which hosts the guest may reach,
which Mac ports it may connect to, and which of your environment variables
it may read. Every project belongs to one workspace, chosen by where the
project lives on your Mac, never by the project.

A project's `sbox.yaml` can still ask for these things, but only inside
what its workspace allows. Anything else stops the session before it boots,
with a line saying what to add and where.

```yaml
# ~/.config/sbox/config.yaml
egress: [api.anthropic.com]                 # every project, every workspace (as today)
secrets: {CLAUDE_CODE_OAUTH_TOKEN: env:CLAUDE_CODE_OAUTH_TOKEN}

workspaces:
  client-a:
    paths: [~/work/client-a/]               # projects under here are in client-a
    egress: ["*.client-a.com"]              # granted: every client-a project gets these
    mounts: [{host: ~/.config/client-a/gitconfig, guest: /home/runner/.gitconfig}]
    secrets: {GITHUB_TOKEN: env:CLIENT_A_GITHUB_TOKEN}
    allow:                                  # the ceiling: what a client-a project may ask for
      egress: [registry.npmjs.org, pypi.org, files.pythonhosted.org, "*.github.com"]
      services: [5432, 6379]
      mounts: [~/work/client-a/shared/]     # read-only unless listed under rw
      rw: [~/work/client-a/out/]
      secrets: [CLIENT_A_NPM_TOKEN]         # Mac variables a project may read
  personal:
    paths: [~/src/]
    allow:
      egress: ["*"]                         # anything: you trust your own repos
      services: ["*"]
```

```yaml
# ~/work/client-a/api/sbox.yaml: inside the ceiling, so it starts
egress: [registry.npmjs.org]
services: [5432]
secrets: {NPM_TOKEN: env:CLIENT_A_NPM_TOKEN}
```

## Why

Today the project file grants itself permissions. `sbox dev
github.com/someone/repo` downloads the repository and reads the
`sbox.yaml` inside it, so a stranger's repository can say:

```yaml
mounts: [{host: ~/.ssh, guest: /home/runner/.ssh}]
secrets: {X: env:AWS_SECRET_ACCESS_KEY}
egress: [evil.example]
```

and sbox copies your SSH keys and AWS secret into a guest that may send them
to `evil.example`. The same holds for any repository you clone and open with
`sbox dev`. `sbox scan` never reads a target's file for exactly this reason;
dev mode trusts it.

The second problem is reach. Lists only append: a mount or egress host in
your file reaches every project, and a project can't take it away. There is
no way to say "client A's repositories may reach client A's hosts and
nothing of client B's".

Each project already has its own session, home drive and network namespace,
so projects are isolated from each other. Workspaces decide what each one
may reach on the outside.

## What exists

- `loadConfig(userFile, projectFile)` merges two files: lists append, maps
  merge with the project winning, scalars replace. `check()` validates each
  file; `image.packages` is already refused in a project file.
- `devHost.devConfig(dir)` loads the config, reads the secrets from the
  Mac's environment and resolves the mounts' host paths; `devMain`,
  `setupEnvMain` and `sbox sync` go through it, `configMain` through
  `devHost.loadConfig`.
- `sbox run` and `sbox scan` read your file and `-config`, never the
  target's (`toolConfig`).
- `devPreflight` prints `BLOCKED` lines before a session boots; the
  `egressAllows` and `hostMatches` helpers already match hosts against
  egress patterns.
- Per-project state lives in `~/.local/share/sbox/projects/<id>/`
  (`base.json`, `session.json`); `sbox clean` removes it.
- Sessions apply `egress`, `ports`, `services`, `mounts`, `permissions` and
  `mcp` live on reattach (`applyLive`).

## Design

### Which keys a workspace governs

A key needs a grant when it reaches outside the VM:

| Key | Reaches | Governed |
|---|---|---|
| `mounts` | your Mac's files (and with rw, writes them back) | yes |
| `secrets` | your Mac's environment | yes |
| `egress` | the internet | yes |
| `services` | your Mac's localhost ports (databases, admin panels) | yes |
| `ports` | a guest port shown on your Mac's localhost | no: it publishes, it doesn't reach in |
| `dev.*`, `permissions`, `mcp`, `vm`, `timeouts`, `prefetch` | inside the VM | no |
| `image.packages`, `lima` | the shared image and VM | already your file only |

MCP servers need no rule of their own: a remote one needs its host in
egress, and a stdio one runs inside the guest.

### Membership: decided by path, in your file

`workspaces.<name>.paths` lists folders on your Mac. A project belongs to
the workspace with the longest path that contains it, after `~` expansion
and symlinks are resolved on both sides. No workspaces are merged: one
project, one workspace. A project under no workspace's paths is in
`default`.

A project never names its own workspace: that would let a downloaded
repository pick the most generous one. You can override the choice for one
run with `-workspace <name>` on `dev`, `run`, `sync`, `setup-env` and
`config`; the flag is yours, not the project's.

`sbox dev github.com/o/r` copies the repository into a folder under the
current directory, which decides its workspace like any other folder. Run
it from `~/src` and it's in `personal`; from `~/Downloads` it's in
`default`.

### The three layers

The effective config for a project is built in this order:

1. **Global grants:** the top-level keys in your file (`egress`, `mounts`,
   `secrets`, `services`), applied in every workspace as today. Keep here
   what every project needs, the agent's own host and token.
2. **Workspace grants:** `workspaces.<name>.egress|mounts|secrets|services`,
   applied to every project in the workspace without being asked for.
3. **Project requests:** the same keys in the project's `sbox.yaml`, each
   checked against what's already granted or what the workspace's `allow`
   lists. Everything else in the project file applies as today.

`default` is an ordinary workspace: you can give it grants and an `allow`
under `workspaces.default`. With none, a project outside every workspace
gets the global grants and may ask for nothing more.

### Matching a request against `allow`

| Request | Covered by an `allow` entry that is |
|---|---|
| egress `host:port` | the same entry; `*.domain:port` with the host under domain; `host:*`; `*` |
| egress `*.domain:port` | `*.domain` or a wider wildcard (`*.parent`), same port or `*`; `*` |
| service `port` | the same port; `*` |
| mount, read-only | a path in `allow.mounts` or `allow.rw` equal to or containing the request's host path (resolved) |
| mount, rw | a path in `allow.rw` equal to or containing it |
| secret `NAME: env:VAR` | `VAR` in `allow.secrets` (the guest name is the project's choice; what matters is which Mac variable is read) |

A request also passes when a grant (global or workspace) already covers it:
asking for `api.anthropic.com` that every workspace grants is harmless.

### Refusal

All requests are checked before anything boots, and every miss is printed
at once, in the preflight's format, with the exact key to add:

```
BLOCKED mount   ~/.ssh → /home/runner/.ssh  not allowed in workspace default → workspaces.default.allow.mounts: [~/.ssh]
BLOCKED secret  X ← env:AWS_SECRET_ACCESS_KEY  not allowed in workspace default → workspaces.default.allow.secrets: [AWS_SECRET_ACCESS_KEY]
BLOCKED egress  evil.example:443  not allowed in workspace default → workspaces.default.allow.egress: [evil.example]
sbox: ./repo asks for more than workspace default allows; nothing was started
```

Without a terminal, sbox stops there (exit 1). With one, it offers the
escape hatch below.

### The escape hatch: approve once, for this project

In a terminal, after the BLOCKED lines:

```
sbox: allow these for ./repo, and remember? [y/N]
```

`y` records the exact entries in `projects/<id>/approved.json` on the Mac,
next to the sync base, and the session starts. The next `sbox dev` on that
project doesn't ask again for the same entries. A changed `sbox.yaml` that
asks for something new asks again, for the new entries only. `sbox clean`
forgets every approval, and `sbox config` lists them.

This is for a single repository you trust. A rule for many repositories
belongs in a workspace's `allow`.

### Live changes

Reattaching to a running session reruns the check on the current files.
A newly allowed request applies live as today (egress, services, mounts); a
refused one is printed as BLOCKED and the session keeps what it booted with.
In a terminal, the same approve-once question is asked.

### `sbox run` and `sbox scan`

`sbox run <dir>` reads no project file, so it has no requests to check. It
takes the global grants plus the target folder's workspace grants (by the
same path rule, or `-workspace`), so a run in `~/work/client-a/x` gets
client A's mounts and egress and nothing of `personal`'s. `-config` stays
your own file, added after them.

`sbox scan` is unchanged: it scans untrusted code with your file only, and
workspaces don't apply.

### Seeing it

`sbox config [dir]` starts with the workspace and why:

```
# workspace client-a (/Users/you/work/client-a/ contains this folder)
egress: # user, workspace client-a, project
  - api.anthropic.com:443
  - "*.client-a.com:443"
  - registry.npmjs.org:443
```

Each key's comment says where it came from (`user`, `workspace <name>`,
`project`, `approved <date>`), and project requests that would be refused
come out as the same BLOCKED lines, so the file can be fixed before a
session is started.

### Migration

This changes behaviour for projects that set `mounts`, `secrets`, `egress`
or `services` in their own `sbox.yaml`: with no workspaces, they're in
`default`, which allows nothing, so their first `sbox dev` lists BLOCKED
lines with the keys to add. That's intended: it is exactly the hole being
closed. Two quick ways through:

- Approve once in the terminal, per project.
- Give the folder you keep your own code in a workspace with a broad
  `allow`, like `personal` above.

The README's dev-mode section and `sbox init`'s starter file explain the
split: the project file asks, your file grants.

## Code touchpoints

| Area | Today | Change |
|---|---|---|
| Config struct | `config` with the top-level keys | `Workspaces map[string]workspace` (user file only, refused in a project or `-config` file for dev); `workspace{Paths, Egress, Mounts, Secrets, Services, Allow{Egress, Services, Mounts, RW, Secrets}}` |
| Loading | `loadConfig(user, project)` merges both | `loadLayers(user, project)` returns the user config, the project config, and the project's raw requests; `effectiveConfig(layers, ws, approved)` builds the merged config and the misses |
| Membership | none | `workspaceFor(dir, cfg, override)`: resolved paths, longest prefix, `default` otherwise |
| Matching | `egressAllows`, `hostMatches` | `allowsEgress`, `allowsMount`, `allowsSecret`, `allowsService` per the table, each with a table test |
| Refusal | `devPreflight` lines | `requestBlocked(miss, ws)` lines; `devConfig` returns them, and the callers print and stop |
| Approvals | none | `projects/<id>/approved.json`: entries and the date approved; `loadApproved`, `saveApproved`; `askApprove(in, out)` beside `askKeep` |
| Commands | `dev`, `sync`, `setup-env`, `config`, `run` | `-workspace <name>` on each; `run` takes the workspace's grants |
| Live apply | `configChanged`, `applyLive` | the check runs first; refused keys stay as booted |
| `sbox config` | sources `user` and `project` | adds `workspace <name>` and `approved`, the header line, and BLOCKED lines for misses |
| `sbox clean` | removes `projects/` | already removes `approved.json` with it |
| Docs | dev-mode, config, README | a Workspaces section in the README and `docs/config.md`; `sbox init`'s starter file says where grants go |

## Commits

- [x] **1. Config.** The `workspaces` key: struct, strict decoding,
    validation (names `[a-z0-9-]+`, `default` reserved but optional; paths
    absolute or `~/`; `allow` entries in the same forms as their keys, plus
    `*` for egress and services). Refused outside your user file. Tests:
    a full file, each bad form, a workspace in a project file.
- [x] **2. Membership.** `workspaceFor`: `~` expansion, symlinks resolved on
    both sides, longest prefix, `default`, the `-workspace` override, and
    an override naming no workspace refused. Table test with nested and
    symlinked folders.
- [x] **3. Matching.** `allowsEgress|Mount|Secret|Service` per the table,
    and grants covering a request. Table tests, including `*.a.com` vs
    `*.b.a.com`, ports, a mount under a symlinked allowed folder, rw asked
    under a read-only allowance.
- [x] **4. The effective config and refusal.** `effectiveConfig` builds the
    three layers; `devConfig` returns the misses as BLOCKED lines and
    `devMain`, `setupEnvMain` and `syncMain` stop before booting. Tests
    with fakes: the downloaded-repo case above refused with all three
    lines and nothing started; an allowed request applied; workspace grants
    reaching the job without a request.
- [x] **5. Approve once.** `approved.json`, the terminal question, asking
    again only for new entries, `sbox clean` forgetting them. Tests: a
    first run asked and recorded, a second not asked, a new egress host
    asked alone, no terminal refused.
- [x] **6. Live changes, `sbox run`, `sbox config`.** The check on reattach
    (refused keys left as booted); `run` taking the target's workspace
    grants; `sbox config`'s header, sources and BLOCKED lines. Tests for
    each.
- [x] **7. Docs and verification.** README Workspaces section, the
    dev-mode, config and hardening plans, `sbox init`'s starter file,
    `MILESTONE.md`; the verification below on the real VM.

## Verification

Checked 2026-10-28: `TestWorkspacesOnVM` on the real VM, unit tests
with fakes, and by hand in a terminal.

- [x] A repository whose `sbox.yaml` mounts `~/.ssh`, reads
    `AWS_SECRET_ACCESS_KEY` and allows `evil.example`, opened with `sbox dev`
    from `~/Downloads`: three BLOCKED lines, no session, nothing copied
    (`sbox ps` empty, no work drive). Without a terminal, exit 1.
    (`TestWorkspacesOnVM`; exit 1 by hand.)
- [x] The same repository opened from a folder in `personal` (egress `*`):
    egress passes, the mount and the secret are still refused.
    (`TestWorkspaceFlagLiveRunConfig`, through `-workspace personal`.)
- [x] A project in `client-a` that asks for `registry.npmjs.org` and
    service 5432: starts, and `npm ci` and `psql` work; asking for
    `pypi.org` instead: refused with `workspaces.client-a.allow.egress:
    [pypi.org]`. (On the VM with `api.github.com` and `example.com` as the
    allowed hosts, `curl` answered; the service and `pypi.org` refusal in
    `TestDevMainWorkspaces` with fakes.)
- [x] client-a's granted mount and token reach a client-a project's guest
    without the project asking; a `personal` project doesn't get them.
- [x] Approving once in a terminal: the session starts; the next `sbox dev`
    doesn't ask; adding a host to `sbox.yaml` asks for that host alone;
    `sbox clean` makes it ask again. (By hand with `expect`;
    `TestDevMainApproveOnce`.)
- [x] Under a running session: adding an allowed egress host to `sbox.yaml`
    applies on reattach; adding a refused one prints BLOCKED and the
    session keeps its egress.
- [x] `sbox run ~/work/client-a/x -- …` gets client-a's grants; the same
    command on a folder in `personal` doesn't.
- [x] `sbox config` in a client-a project names the workspace and why, and
    each key's source; a refused request shows as BLOCKED there too.
- [x] Unchanged without workspaces for projects whose `sbox.yaml` asks for
    none of the four keys: the existing dev-mode VM tests pass as they are.
    (They pass, now with a user config that allows what their project
    files ask for.)

## Considered and left out

- **The project naming its workspace** (`workspace: client-a` in
  `sbox.yaml`): a downloaded repository would name the most generous one.
- **Workspaces merged along nested paths:** one workspace per project keeps
  the result predictable; nest by giving the inner folder its own
  workspace.
- **A grant-only model** (no `allow`; projects ask for nothing): simpler,
  but every project in a workspace would get the union of what any of them
  needs. `allow` keeps each project to what it asks for.
- **Gating `ports`:** a guest port shown on your Mac's localhost publishes
  rather than reaches in; revisit if a case comes up (a port clashing with
  a local OAuth callback, say).
- **A separate Lima VM or network per workspace:** projects already get
  their own microVM and namespace; workspaces are about permissions, not
  isolation between projects.
- **Time-limited approvals or grants:** add when someone asks.
- **Workspaces for `sbox scan`:** scan reads no target file, and its
  permissions are yours already.

## Out of scope

- Per-workspace guest images (`image.packages` stays global; a rebuild
  takes minutes and the image is shared).
- Teams sharing a workspace definition: it lives in your own file; a
  shared file would need its own trust story.
