#!/usr/bin/env bash
# Provisions the sbox VM: Firecracker, the guest kernel and the guest rootfs.
# Runs as root inside the VM. Idempotent: downloads are cached and checked
# against pinned SHA-256 sums, the rootfs is rebuilt only when its inputs change.
set -euo pipefail

DIR=/opt/sbox
ROOTFS_REVISION=4 # bump when the rootfs recipe below changes
GUEST_PACKAGES=ca-certificates,libstdc++6,curl,wget,netcat-openbsd,iproute2,iputils-ping,python3,python3-pip,python3-venv,strace,git,procps,xz-utils

mkdir -p "$DIR/cache" "$DIR/bin"
cd "$DIR"

step() { echo "==> $*"; }

fetch() { # name url sha256
	local out="cache/$1"
	if [[ -f "$out" ]] && echo "$3  $out" | sha256sum -c --quiet - >/dev/null 2>&1; then
		return
	fi
	step "download $1"
	curl -fsSL --retry 3 -o "$out.tmp" "$2"
	if ! echo "$3  $out.tmp" | sha256sum -c --quiet - >/dev/null 2>&1; then
		rm -f "$out.tmp"
		echo "checksum mismatch for $1 ($2)" >&2
		exit 1
	fi
	mv "$out.tmp" "$out"
}

step "packages"
apt-get update -qq
DEBIAN_FRONTEND=noninteractive apt-get install -y -qq mmdebstrap e2fsprogs acl iproute2 >/dev/null
# The jailer drops Firecracker to this user.
id -u sbox-vm >/dev/null 2>&1 || useradd --system --no-create-home --shell /usr/sbin/nologin sbox-vm
mkdir -p /var/lib/sbox/runs /srv/sbox-jail

fetch firecracker.tgz "$FIRECRACKER_URL" "$FIRECRACKER_SHA256"
fetch vmlinux "$KERNEL_URL" "$KERNEL_SHA256"
fetch node.tar.xz "$NODE_URL" "$NODE_SHA256"
fetch uv.tar.gz "$UV_URL" "$UV_SHA256"

step "firecracker"
tmp=$(mktemp -d)
tar -xzf cache/firecracker.tgz -C "$tmp"
install -m 0755 "$tmp"/release-*/firecracker-v*-aarch64 bin/firecracker
install -m 0755 "$tmp"/release-*/jailer-v*-aarch64 bin/jailer
rm -rf "$tmp"
install -m 0644 cache/vmlinux vmlinux

# The guest image: Ubuntu minbase from the signed archive, node and uv from
# the pinned tarballs, an unprivileged runner user, and the doctor's hello init.
stamp="rootfs.r${ROOTFS_REVISION}-${NODE_SHA256:0:12}-${UV_SHA256:0:12}"
if [[ -f rootfs.ext4 && -f "$stamp" ]]; then
	step "rootfs up to date"
	exit 0
fi

step "rootfs (a few minutes)"
root=$(mktemp -d)
trap 'rm -rf "$root"' EXIT
mmdebstrap --quiet --variant=minbase --components=main,universe --include="$GUEST_PACKAGES" \
	noble "$root" http://ports.ubuntu.com/ubuntu-ports
tar -xJf cache/node.tar.xz -C "$root/usr/local" --strip-components=1 \
	--exclude='*/include' --exclude='*/share/doc' --exclude='*/share/man'
tar -xzf cache/uv.tar.gz -C "$root/usr/local/bin" --strip-components=1
chroot "$root" useradd --create-home --uid 1000 --shell /bin/bash runner
mkdir -p "$root/drive"

# Stub init: mounts the run's drive and hands over to the init it carries,
# so run logic can change without rebuilding this image.
cat >"$root/sbin/sbox-init" <<'INIT'
#!/bin/sh
mount -t proc proc /proc
mount -t sysfs sys /sys
mount -o ro /dev/vdb /drive && exec /bin/sh /drive/sbox/init.sh
echo "SBOX-ERROR no run drive"
echo 1 >/proc/sys/kernel/sysrq
echo b >/proc/sysrq-trigger
INIT
chmod 0755 "$root/sbin/sbox-init"

cat >"$root/sbin/sbox-hello" <<'EOF'
#!/bin/sh
# Doctor's boot check: report the toolchain and the network boundary, then power off.
mount -t proc proc /proc
mount -t sysfs sys /sys
ip link set lo up
if ip link show eth0 >/dev/null 2>&1; then
	ip addr add 172.16.0.2/30 dev eth0
	ip link set eth0 up
	ip route add default via 172.16.0.1
	ping -c1 -W2 172.16.0.1 >/dev/null 2>&1 && echo "SBOX net-host=ok" || echo "SBOX net-host=fail"
	ping -c1 -W2 1.1.1.1 >/dev/null 2>&1 && echo "SBOX net-internet=reachable" || echo "SBOX net-internet=blocked"
fi
echo "SBOX node=$(node --version 2>&1)"
echo "SBOX python=$(python3 --version 2>&1 | cut -d' ' -f2)"
echo "SBOX uv=$(uv --version 2>&1 | cut -d' ' -f2)"
echo "SBOX strace=$(strace -V 2>&1 | head -1 | awk '{print $NF}')"
# No systemd, so no reboot(8): ask the kernel. Firecracker exits on reboot.
echo 1 >/proc/sys/kernel/sysrq
echo b >/proc/sysrq-trigger
EOF
chmod 0755 "$root/sbin/sbox-hello"

rm -rf "$root"/var/cache/apt/archives/*.deb "$root"/var/lib/apt/lists/*
size_mb=$(($(du -sm "$root" | cut -f1) * 13 / 10 + 256))
rm -f rootfs.ext4.tmp
truncate -s "${size_mb}M" rootfs.ext4.tmp
mkfs.ext4 -q -d "$root" -F rootfs.ext4.tmp
mv rootfs.ext4.tmp rootfs.ext4
rm -f rootfs.r*-*
touch "$stamp"
step "rootfs ${size_mb}M"
