# Prerequisites: sbox setup installs Lima itself (new)

## Goal

`sbox setup` works on a Mac that has nothing but the sbox binary. Today it
assumes `limactl` is on `PATH`, and doctor's hint is `brew install lima`.

## Design

**Lima as a pinned artifact.** Like Firecracker, Node and uv: one Lima
release, its URL and SHA-256 in `setup.go`, taken from the release's own
`SHA256SUMS` file at implementation time. The archive
(`lima-<version>-Darwin-arm64.tar.gz`) holds `bin/limactl` and
`share/lima/` (guest agents and templates, which `limactl` finds relative
to its own path). It is extracted to `~/.local/share/sbox/lima/` on the
host. No Homebrew, no sudo, nothing outside that folder. A mismatched sum
aborts and keeps nothing, as provisioning does.

**Which limactl runs.** `limactlPath()` resolves once per process: the
pinned copy under `~/.local/share/sbox/lima/bin` when present, otherwise
`limactl` from `PATH`. Every `exec.Command("limactl", ...)` in `lima.go`,
`run.go` and `doctor.go` goes through it. A user with their own Lima keeps
using it unless it is older than the pin's minimum, in which case setup
says so and installs the pinned copy beside it. Lima keeps its instances in
`~/.lima` either way, so both binaries see the same `sbox` VM.

**Host preflight**, before anything is downloaded, each with a plain
message and exit 125:

- Apple Silicon: `uname -m` is `arm64`.
- Nested virtualization needs an M3 or later chip and macOS 15 or later:
  `sysctl -n machdep.cpu.brand_string` and `sw_vers -productVersion`. The
  VM's own `/dev/kvm` check in doctor stays the authority; this only turns
  a confusing late failure into an early sentence.
- Free disk for the VM disk plus the image: `syscall.Statfs` on the home
  directory against `lima.disk_gib` plus 5 GiB.

**Doctor.** The `limactl` check reports which binary it used, pinned or
from `PATH`, and its version. The hint for a missing one becomes `sbox setup`.

## Commits

- [ ] **1. Preflight.** `preflight.go`: chip, macOS version, disk; the
    command outputs are parsed by functions with table tests on captured
    strings (M1, M3, M5, an Intel string, macOS 14 and 26).
- [ ] **2. `limactlPath()`.** Resolution order with tests on a temp home
    and a fake `PATH`; every call site switched to it.
- [ ] **3. Pinned Lima download.** The artifact entry, download with
    checksum, safe extraction (the same rules as the GitHub targets plan),
    minimum-version check against a user's own Lima. Tests with
    `httptest.Server` serving a small tarball.
- [ ] **4. Verify and document.** On a Mac without Lima: `sbox setup`
    installs it, creates the VM, doctor passes. README's Requirements drop
    "Lima installed".

## Out of scope

- Intel Macs and Linux hosts.
- Updating Lima in place: a new pin is a new download beside the old one.
