# Firecracker snapshots: built, measured, dropped

## Status: closed

A session resumed from a snapshot works under nested virtualization, and it is
slower than a cold boot, for its whole life. Don't build it again without a
way past the reason below. The code is in git history: commit 42b192b, reverted
by aa688da.

## What was measured

M4 Pro, 2 vCPUs, a small project (2026-11-03), with transparent huge pages on
in the Lima VM ([what those do](../MILESTONE.md)):

| | Cold boot | Restored |
|---|---|---|
| `sbox dev <dir> -- true`, the whole run | 1.1 s | 3.2 s |
| First touch of 512 MiB in the guest | 0.2 s | 22 s |

Why: Firecracker maps a restored guest's memory from the snapshot's file. Huge
pages don't apply to a file mapping, so every page the guest first touches
faults at 4 KiB, through two layers of virtualization, about 180 µs each. A
booted guest's memory is anonymous, so it gets 2 MiB pages and none of that.

Firecracker's way out is hugetlbfs memory with a userfaultfd handler of our own
to load the snapshot. With a whole cold-boot run at 1.1 s, there is little left
to win.

## What worked, should it matter again

- A guest parked in a stub on a placeholder drive, warming the page cache, then
  mounting in a loop until the drive under it is a real one: a restored guest
  needs no signal, the snapshot holds nothing of any session's, and the image
  doesn't change.
- A restored guest wakes with the snapshot's clock and random state: it has to
  set its clock and reseed (`RNDRESEEDCRNG`) before anything else.
- The guest remembers its drives' sizes, so they must be fixed.
