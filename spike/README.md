# Milestone 0: feasibility spike

Question: can Firecracker microVMs run on this Mac (Apple M5 Pro, macOS 26.7)?

**Answer: yes.**

| Layer | What ran |
|---|---|
| Host | macOS, Lima 2.2.1 |
| Lima VM `sbox` | vz, `nestedVirtualization: true`, Ubuntu 24.04 arm64, 4 CPU / 6 GiB, no host mounts; `/dev/kvm` present |
| Firecracker | v1.17.0 aarch64, release checksum verified |
| Guest | CI kernel 6.1.155 + Ubuntu 24.04 rootfs from Firecracker's `v1.15` CI artifacts (the newest published) |

Boot to clean exit: **2.6 s** (kernel up at ~1.7 s, init as PID 1, `reboot -f`
ends Firecracker with exit code 0). Repeated three times, same result.

```sh
limactl shell sbox -- bash -s < spike/01-install.sh   # Firecracker, kernel, rootfs
limactl shell sbox -- bash -s < spike/02-boot.sh      # boot hello microVM, time it
```

## Findings for the real implementation

- Firecracker's serial console reads stdin: always run it with `</dev/null`,
  or a piped script gets swallowed by the guest.
- The CI rootfs has python3 and strace but no node. `sbox setup` must build
  its own rootfs (node, npm, python, uv, strace, the sbox init).
- CI artifacts lag releases (v1.15 while Firecracker is at v1.17), so
  `sbox setup` should pin artifact URLs and checksums instead of listing the
  bucket.
- The spike opens `/dev/kvm` to the lima user; the real runner uses the jailer.
- Not yet tested: tap networking (milestone 3).
