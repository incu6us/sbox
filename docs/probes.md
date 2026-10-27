# Probe suite: regression tests over real microVM runs (new)

## Goal

Every plan verifies itself with probe skills that today exist only as
sentences in `MILESTONE.md` ("a probe hook that reads `~/.aws/credentials`
then looks up a host: BLOCK"). Check them in, give each an expected
result, and run them all with one command when the VM is up. Each later
item's verification step becomes "the suite still passes, plus these new
probes".

## What exists

- `go test ./...` covers parsers, planners and report rendering with no VM.
- Doctor's run probe is the only automated real run (`doctor.go: probe`).
- The calibration numbers in milestone 7 were produced by hand.

## Design

**Layout.** `testdata/probes/<name>/` holds a complete target directory
(the skill) plus one file sbox ignores, `expected.txt`:

```
verdict REVIEW
rule credential-read
rule dns-lookup
not  canary-exfiltration
```

`verdict` is required; `rule` lines must appear among the findings, `not`
lines must not. Nothing else is asserted, so a wording change in a detail
string doesn't break the suite, and a new rule firing on an old probe does
(by design: that is a calibration change to look at).

**The probes**, one per rule family, each a few lines of shell or JSON:

| Name | What it does | Expected |
|---|---|---|
| `pass-hook` | a hook that reads its own files and exits 0 | PASS, no rules |
| `credential-read` | hook reads `~/.aws/credentials` | REVIEW, `credential-read` |
| `credential-then-dns` | reads it, then looks up a host | BLOCK, `possible-exfiltration` |
| `canary-dns` | puts the fake AWS secret in a DNS name | BLOCK, `canary-exfiltration` |
| `canary-argv` | passes it to `curl` | BLOCK, `canary-exfiltration` |
| `raw-connect` | connects to 1.1.1.1:443 | REVIEW, `connect` |
| `agent-config-write` | writes `~/.claude/settings.json` | REVIEW, `agent-config-write` |
| `persistence` | appends to `~/.bashrc` | REVIEW, `persistence` |
| `temp-exec` | copies a script to `/tmp` and runs it | REVIEW, `temp-exec` |
| `environ-read` | reads `/proc/1/environ` | REVIEW, `environ-read` |
| `mcp-handshake` | a stdio MCP server in node answering `tools/list` | PASS |
| `npm-postinstall` | `package.json` with a postinstall that curls | REVIEW, `network-tool` |
| `prefetch-refused` | a `github:` dependency | REVIEW, `prefetch-refused` |
| `timeout` | a hook that sleeps past the per-entry timeout | PASS or REVIEW per current rules, `timeout` |
| `no-entry-points` | a SKILL.md with no hooks (until item 3 lands) | exit 0, no VM |

Later items add their own rows: `script-help`, `unscanned-binary`,
`echo` and `probe-agent` for agent mode (those two are skipped unless an
API key is set and `-tags agent` is given, since they cost tokens).

**The runner.** `probes_test.go` behind `//go:build vm`:

```go
func TestProbes(t *testing.T) {
    if ok, _ := doctor(ctx, io.Discard); !ok { t.Skip("sbox VM not ready") }
    for each dir in testdata/probes:
        out := run([]string{"scan", "-json", dir}, &stdout, &stderr)
        compare verdict, rules against expected.txt
}
```

`go test -tags vm -run TestProbes ./...` runs them in sequence (one VM at a
time keeps timing readable); about 15 probes at 5 to 10 s each. The test
prints the full report of any probe that fails, so a calibration change is
read from the test output.

**Calibration set.** A second test, `TestInstalled`, behind the same tag
and `-installed`, scans the folders `skills-audit -installed` lists and
asserts only that none errors and none is BLOCK, with the verdict table
printed. It replaces the hand-made table in milestone 7.

## Commits

- [ ] **1. The probes and `expected.txt` reader.** Fifteen directories, the
    parser for `expected.txt` with a unit test, and a check that each
    probe's expectation matches what `sbox scan` prints today. Any mismatch
    is a bug or a calibration change, and is fixed or recorded before the
    commit.
- [ ] **2. The runner.** `probes_test.go` with the `vm` tag, the doctor
    skip, per-probe subtests, report on failure. README's Development
    section: how to run it.
- [ ] **3. `TestInstalled`.** The calibration test and the milestone table
    regenerated from it.

## Verification

- [ ] `go test ./...` without the tag is unchanged and needs no VM.
- [ ] `go test -tags vm -run TestProbes ./...` passes on a fresh `sbox setup`.
- [ ] Breaking a rule on purpose (comment out `credential-read`) fails
    exactly the probes that depend on it.

## Out of scope

- Running the suite in CI: there is no arm64 KVM runner in the project.
- Fuzzing the trace parser; `scan_test.go` covers it with samples.
