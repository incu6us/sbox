# sbox

Go CLI that runs projects and agents in Firecracker microVMs inside a Lima
VM on macOS. Plans and their status checkboxes: `docs/README.md`. History
and verified numbers: `MILESTONE.md`.

## Strict TDD (required for every change)

Red, green, refactor, one small behaviour at a time:

1. **Red.** Write one failing test for the next behaviour. Run it and see
   it fail for the expected reason: a wrong value or a missing function,
   not an unrelated compile error or a broken fixture.
2. **Green.** Write the least code that makes it pass. Run the whole suite.
3. **Refactor** with the suite green. No new behaviour in this step.

No production code without a failing test that asks for it. A bug fix
starts with a test that reproduces the bug. Deleting a test to get green
is not allowed; changing one needs a reason in the commit message.

How each layer is tested:

- **Go:** table tests in `*_test.go`, `t.Parallel()`, standard library
  `testing` only. Code that calls `limactl` takes the call as a function
  parameter so tests pass a fake.
- **VM-side shell (`scripts/*.sh`):** run from Go tests with `bash`, path
  roots moved into a temp dir through `SBOX_BASE`, `SBOX_STATE` and
  `SBOX_JAIL`, and fake commands (`ip`, `curl`, `setsid`, the jailer, ...)
  first on `PATH`. New shell code must work in bash 3.2 (macOS, where unit
  tests run) and bash 5 (the VM).
- **Python helpers:** extend their `--selftest`.
- **Real VM:** integration tests behind `//go:build vm`, run with
  `go test -tags vm ./...` after `sbox setup`. A change to VM-side scripts
  is not done until those pass. When they could not be run, say so.

Report results as they happened: the command, and pass or fail.

## Conventions

- Go standard library only.
- Everything from inside the guest is untrusted: results travel in
  nonce-marked lines, and the host never runs code that came from the guest.
- Before calling work done: `gofmt -l .` prints nothing, `go vet ./...` and
  `go test ./...` pass.
- When a planned commit lands, tick its checkbox in `docs/`. When a whole
  item lands, update `MILESTONE.md` and `README.md` in the same commit.
