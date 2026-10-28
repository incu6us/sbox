# GitHub URLs as targets (v2)

## Goal

`sbox scan https://github.com/owner/repo` and `sbox run <url> -- cmd` work
on a repository, a ref, or a folder inside one, so marketplaces and plugins
can be scanned before anything is cloned by hand. The milestone's v1 says
"fetch locally first"; this plan does exactly that, on the host, with the
standard library, and without running git.

## What exists

- `scanMain` and `runMain` take a local directory; `runVM` checks it is one
  and `writeRunTar` packs it (256 MiB cap, symlinks kept as links).
- The guest never sees the host; the fetch is host-side by necessity, since
  entry point discovery (`findEntryPoints`, `planPrefetch`) runs on the host.

## Design

**Accepted forms.**

| Argument | Meaning |
|---|---|
| `https://github.com/o/r` | default branch, whole repo |
| `https://github.com/o/r/tree/<ref>` | that ref |
| `https://github.com/o/r/tree/<ref>/<path>` | that folder at that ref |
| `github.com/o/r[@ref][/path]` | same, short |
| `o/r@ref` | not accepted: too easy to confuse with a local path |

Anything that doesn't parse is treated as a local path, as today.

**Fetch without git.** GitHub serves `https://codeload.github.com/o/r/tar.gz/<ref>`
for any branch, tag or commit. `target.go: fetchTarget(ctx, url)` downloads
it with `net/http` (30 s timeout, 256 MiB cap on the gzip stream, same
constant as the tar cap), extracts with `compress/gzip` and `archive/tar`
into `os.UserCacheDir()/sbox/targets/<o>/<r>/<ref>/`, and returns the
directory plus the commit the tarball names (the top-level folder is
`<r>-<short sha>`; the `ETag` header carries the full sha). No git, so no
hooks, filters, smudge, LFS or submodule code runs on the host. Submodules
are empty folders in the tarball; the report says so when `.gitmodules`
exists.

Extraction rules: paths with `..` or a leading `/` are refused; symlinks are
created as links (the guest copies them as links too) but a link whose
target escapes the folder is refused; device and special entries are
skipped; mode bits are kept for the executable bit only.

**Ref resolution.** With no ref, the tarball of `HEAD` is the default
branch; its sha from `ETag` is what the report shows, so a scan is
reproducible by `.../tree/<sha>`. No API calls, so no rate limit and no
token.

**Caching.** A fetched ref is reused on the next run if it is a 40-char sha;
branch and tag names are fetched again (they move). `-refetch` forces it.
Cache entries older than 30 days are removed on the next fetch.

**Report.** The header line `N entry points in <dir>` becomes `N entry
points in github.com/o/r@<sha>/<path>`; JSON gets `"target": {"url",
"commit", "path"}`. Findings paths are unchanged (they are guest paths).

**Security of the fetch itself.** The download goes to a per-target empty
folder under the user's cache, never the working directory, and nothing in
it is executed on the host. The host-side readers (`findEntryPoints`,
`planPrefetch`, the SKILL.md parser) already treat file content as data.

## Commits

- [x] **1. `target.go`.** URL parsing (`parseTarget`), fetch, safe extraction,
    cache path. Tests with `httptest.Server` serving a tarball built in the
    test: the happy path, a `..` entry, an escaping symlink, the size cap,
    a 404 (clear error naming the ref).
- [x] **2. Wire in.** `scanMain` and `runMain` call `resolveTarget(arg)` which
    returns a local dir and a display name; the report header and JSON use
    the display name. `-refetch` flag. README: the URL forms in `sbox scan`
    and `sbox run`.
- [x] **3. Verify and document.** Scan three public plugins by URL (one by
    branch, one by tag, one subfolder of a marketplace repo); compare with
    the local clone's result. `MILESTONE.md` v2 "GitHub URLs as targets" ✅.

Done with these differences: `sbox dev <url>` copies the fetched tree
into a new `./<repo>` folder and works there (sync needs a folder you can
find); the commit comes from the tarball's pax header, not `ETag`. The
30-day cleanup: each fetch marks its entry used and removes the ones
unused for 30 days. `-refetch` on `run` and `scan` downloads even a
commit again (a cache entry someone edited, say), and the JSON `target`
is `{"url", "commit", "path"}`, with the full commit, or `{"path"}` for
a local folder.

## Out of scope

- Private repositories (would need a token on the host; add `GITHUB_TOKEN`
  passthrough only when someone asks).
- Other forges. GitLab has an equivalent archive URL; same function, one
  more pattern, when needed.
- Claude plugin marketplace names (`owner/repo` plus `plugin.json`
  lookup): a later convenience layer on top of this.
