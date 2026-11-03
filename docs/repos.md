# Repos and URLs: control more than hosts (new)

## Goal

`egress` says which hosts the guest may reach. Let it also say which repositories,
and which paths on a host, and whether a repository may be pushed to:

```yaml
repos:
  - github.com/incu6us/sbox-push-check          # clone and push
  - {repo: "github.com/*", push: false}         # clone anything public, push nowhere else
egress:
  - api.anthropic.com                           # host only: passed through, untouched, as today
  - registry.npmjs.org/left-pad                 # a path: decrypted and checked
```

## Why

Allowing `github.com` is broader than it looks. A fine-grained token limits what
*your* credential can push, not what the guest can do with one it brought:
code in the guest can push your files to an attacker's repository, or post a
gist through `api.github.com`, with a token planted in a dependency. Host rules
can't see that: the request is encrypted to the server.

## What exists

- `scripts/sbox-proxy.py` handles `CONNECT host:port`, checks the host and port
  against a rules file (reread when it changes), and splices bytes. It never sees
  a path. It runs as `sbox-vm` in the Lima VM, outside the guest's namespace.
- Raw TCP egress (`github.com:22`) is a relay to one host; nothing in it can
  be checked.
- Guest env reaches the terminal, the hooks and the agent through `sbox/env`; the
  root is read-only, with `/etc/resolv.conf` and `/etc/hosts` bind-mounted from
  tmpfs at boot.
- The guest has `git`, `curl`, Node and Python, and Claude Code as a native
  binary; the Lima VM has `openssl`.
- Workspaces govern what a project may ask for (`workspace.go`).

## Design

### Decrypting only where a rule needs it

A host with a path or repo rule is decrypted; every other host stays a spliced
tunnel, so `api.anthropic.com` and the agent's token are never seen in the clear.

- **A CA per session**, made by `sbox-run` when the config has such rules. Its key
  stays root-only in the session's run folder in the Lima VM, readable by the
  proxy's user and never on the guest's drive. Only its certificate goes to the
  guest.
- **The guest trusts it** through a bundle bind-mounted over
  `/etc/ssl/certs/ca-certificates.crt` (how `resolv.conf` is done; git, curl,
  Python and Go read it), and `NODE_EXTRA_CA_CERTS` for Node. Whether the agent's
  own runtime reads either is for the spike.
- **Leaf certificates** are made per host on first use with `openssl`, for that
  session's CA, and cached in its folder.
- **The proxy** terminates TLS from the guest, reads each request's method and
  path, checks them, and sends allowed requests on over a TLS connection of its
  own that verifies the real server. It offers HTTP/1.1 only, and closes after
  each response (`Connection: close`). ponytail: a new upstream connection per
  request; keep-alive if a git fetch's round trips ever show.

### Rules

- A path rule in `egress`, `host/path`, allows that path and everything under it,
  by whole segments, after normalising: `%2e`, `..`, `//` and a path that isn't
  absolute are refused, never rewritten, since the server may read them
  differently.
- `repos` is the same mechanism with git's and GitHub's paths filled in. For
  `github.com/o/r`:

  | Host | Allowed |
  |---|---|
  | `github.com` | `/o/r`, `/o/r.git` and under: `info/refs`, `git-upload-pack`, and with push, `git-receive-pack` |
  | `api.github.com` | `/repos/o/r` and under |
  | `raw.githubusercontent.com`, `codeload.github.com` | `/o/r` and under |

  `push: false` refuses `git-receive-pack`, both `info/refs?service=` and the POST,
  and on `api.github.com` every method but GET and HEAD. Anything else on those
  hosts is refused: gists, other users, creating a repository.
- **Rules can't be bypassed by the raw route.** With `repos` for a host, raw TCP
  egress to that host (`github.com:22`) is a config error naming the conflict: git
  over SSH can't be checked.
- **Other hosts** behave as today. Case and a default port compare as `egress` does.

### Governance

`repos` joins `egress`, `services`, `mounts` and `secrets`: a project asks, your
file allows or grants, with `allow.repos` patterns, BLOCKED lines before boot and
approve-once. An entry a project asks for with push needs an `allow.repos` entry
that gives push. A host rule the user granted isn't narrowed by a project's path
rule, and a project's path rule doesn't widen anything: the guest gets the union of
what was granted and allowed, and a host with no path rule in that union stays a
tunnel.

### Seeing it

A refused request is a line in the proxy's log, as a refused host is, and a line in
the summary and `sbox watch` with the key to add:
`BLOCKED repo    github.com/evil/x (push)  not in repos → repos: [github.com/evil/x]`.

Live changes: rules reload with the file, as today; a session that started with no
decrypted hosts has no CA, so adding the first one needs `sbox dev -restart`.

## What it gives up

- Decrypted traffic for those hosts is plaintext in the proxy, in the Lima VM on your
  Mac. The token in a decrypted request is visible to that process; the proxy logs
  no headers.
- A tool that pins its certificates fails on a decrypted host. That is the failure
  mode wanted: refused, not bypassed.
- An allowed repository can still receive your files. `repos` narrows where they can
  go; it doesn't judge what's sent.

## Commits

- [ ] **0. Spike.** On the real VM by hand, a CA in the bundle and a decrypting
  proxy: `git clone` and `git push`, `npm ci`, `curl`, and Claude Code talking to a
  decrypted host (`NODE_EXTRA_CA_CERTS`, `SSL_CERT_FILE`, or neither). If the agent
  can't be made to trust the CA, its traffic stays tunnelled and the docs say so.
- [ ] **1. The proxy.** `sbox-proxy.py`: path rules in the rules file, TLS termination,
  request parsing, the verified upstream, streamed bodies, leaf certificates. Extends
  its `--selftest` with a local TLS server: allowed, refused, `..`, `%2e`, a chunked
  POST.
- [ ] **2. The CA and the guest's trust.** `sbox-run.sh`: the CA per session, key off
  the drive. `guest-init.sh`: the bind-mounted bundle. Shell tests with fakes.
- [ ] **3. Config.** `repos`, path rules in `egress`, expansion to rules, the raw-TCP
  conflict, `allow.repos`, BLOCKED and approve-once, `sbox config`.
- [ ] **4. Seeing it.** The refusals in the summary and `sbox watch`.
- [ ] **5. Docs and the real VM.** README, `docs/config.md`, `MILESTONE.md`. Timing of a
  `git clone` against a tunnel.

## Verification

- [ ] With `repos: [github.com/o/allowed]`: clone and push to it work; push to another
  repository with a token that could write there is refused by the proxy, before
  GitHub sees it.
- [ ] `push: false`: clone works, push is refused at `info/refs`; a gist through
  `api.github.com` is refused.
- [ ] `/o/allowed/../evil`, `%2e%2e`, a double slash and `/O/ALLOWED` behave as the
  rules say, against a real server.
- [ ] A host with no path rule is still a tunnel: the guest sees its real certificate.
- [ ] The CA's key is unreadable from the guest and not on its drive; two sessions have
  different CAs; a session's certificate isn't trusted by another.
- [ ] A downloaded repository's `repos` in its own `sbox.yaml` is refused in `default`.

## Considered and left out

- **Decrypting everything:** the agent's token and every host's traffic in plaintext,
  for no rule that asks for it.
- **Git over SSH with a filtering proxy:** needs an SSH man-in-the-middle holding keys.
- **Token-side limits alone:** a planted token isn't yours to limit.

## Out of scope

- Other forges' path tables. `repos` starts with GitHub; path rules work anywhere.
- Inspecting bodies: what's sent, only where.
