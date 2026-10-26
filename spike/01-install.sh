#!/usr/bin/env bash
# Spike step 1: install a pinned Firecracker release and fetch a guest
# kernel + rootfs from Firecracker's CI artifacts. Runs inside the sbox VM.
set -euo pipefail

FC_VERSION=v1.17.0
CI_VERSION=v1.15 # newest guest kernel/rootfs Firecracker publishes; images are not tied to the binary version
ARCH=aarch64
DIR=/opt/sbox
CI_BUCKET=https://s3.amazonaws.com/spec.ccfc.min

sudo mkdir -p "$DIR" && sudo chown "$USER" "$DIR" && cd "$DIR"
sudo usermod -aG kvm "$USER"
sudo setfacl -m "u:${USER}:rw" /dev/kvm 2>/dev/null || sudo chmod 666 /dev/kvm # ponytail: spike only; sbox setup will use the jailer

if [[ ! -x bin/firecracker ]]; then
	base="https://github.com/firecracker-microvm/firecracker/releases/download/${FC_VERSION}"
	tgz="firecracker-${FC_VERSION}-${ARCH}.tgz"
	curl -fsSLO "${base}/${tgz}"
	curl -fsSLO "${base}/${tgz}.sha256.txt"
	sha256sum -c "${tgz}.sha256.txt"
	tar xzf "$tgz"
	mkdir -p bin
	cp "release-${FC_VERSION}-${ARCH}/firecracker-${FC_VERSION}-${ARCH}" bin/firecracker
	cp "release-${FC_VERSION}-${ARCH}/jailer-${FC_VERSION}-${ARCH}" bin/jailer
fi
bin/firecracker --version | head -1

# Pick the newest 6.1 kernel and ubuntu rootfs listed for CI_VERSION.
ci="firecracker-ci/${CI_VERSION}/${ARCH}"
list() { curl -fsSL "${CI_BUCKET}/?prefix=${ci}/$1&list-type=2" | grep -oP "(?<=<Key>)${ci}/$1[^<]*" | grep -vE '\.(config|debug|gz)$' | sort -V | tail -1; }
kernel_key=$(list "vmlinux-6.1")
rootfs_key=$(list "ubuntu-24.04.squashfs")
[[ -n "$kernel_key" && -n "$rootfs_key" ]] || { echo "CI artifacts not found under $ci" >&2; exit 1; }
echo "kernel: $kernel_key"
echo "rootfs: $rootfs_key"
[[ -f vmlinux ]] || curl -fsSL -o vmlinux "${CI_BUCKET}/${kernel_key}"
[[ -f rootfs.squashfs ]] || curl -fsSL -o rootfs.squashfs "${CI_BUCKET}/${rootfs_key}"
ls -la "$DIR"
