#!/usr/bin/env bash
# Runs one sbox job. Reads the run as a tar on stdin (skill/ and sbox/),
# builds its drive, boots Firecracker under the jailer as an unprivileged
# user, streams the serial console to stdout, and removes everything on exit.
#
# Network: each run gets its own network namespace holding only loopback and
# the run's tap device. There is no veth and no route out, so the guest can
# reach the VM side of its tap (172.16.0.1) and nothing else. sbox-dns
# listens there, answers every lookup with NXDOMAIN and logs the names; the
# log is printed after the job, marked with the run's nonce.
#
# Prefetch: if the run carries sbox/prefetch.sh and a prefetch timeout is
# given, a first microVM downloads the skill's dependencies onto an empty
# writable drive. Its only way out is a relay on 172.16.0.1:3128 to a proxy
# that tunnels to the package registries alone. The relay and proxy are
# stopped before the main microVM boots with that drive attached read-only.
#
# Usage (as root): sbox-run <run-id> <timeout-seconds> [prefetch-timeout-seconds]
set -euo pipefail

id=${1:-} timeout=${2:-} prefetch_timeout=${3:-0}
[[ $id =~ ^[a-f0-9]{8,32}$ ]] || { echo "sbox-run: bad run id" >&2; exit 2; }
[[ $timeout =~ ^[0-9]{1,5}$ ]] || { echo "sbox-run: bad timeout" >&2; exit 2; }
[[ $prefetch_timeout =~ ^[0-9]{1,5}$ ]] || { echo "sbox-run: bad prefetch timeout" >&2; exit 2; }

base=/opt/sbox
jail_base=/srv/sbox-jail
run=/var/lib/sbox/runs/$id
netns=sbox-$id

pids=()
cleanup() {
	# Some are already stopped; a failed kill must not abort the cleanup.
	kill "${pids[@]}" 2>/dev/null || true
	# Anything else still in the run's namespace goes too, so nothing a run
	# starts outlives it.
	ip netns pids "$netns" 2>/dev/null | xargs -r kill -KILL 2>/dev/null
	ip netns del "$netns" 2>/dev/null || true
	rm -rf "$run" "$jail_base/firecracker/$id" "$jail_base/firecracker/$id-pre"
	rmdir "/sys/fs/cgroup/firecracker/$id" "/sys/fs/cgroup/firecracker/$id-pre" 2>/dev/null || true
}
trap cleanup EXIT

mkdir -p "$run/drive"
tar -x -C "$run/drive" --no-same-owner --no-same-permissions
nonce=$(cat "$run/drive/sbox/nonce")

# The run's drive: the skill plus the guest init and job, read-only in the guest.
size_mb=$(($(du -sm "$run/drive" | cut -f1) * 13 / 10 + 32))
truncate -s "${size_mb}M" "$run/drive.ext4"
mkfs.ext4 -q -d "$run/drive" -F "$run/drive.ext4"

ip netns add "$netns"
in_ns() { ip netns exec "$netns" "$@"; }
in_ns ip link set lo up
in_ns ip tuntap add dev tap0 mode tap user "$(id -u sbox-vm)"
in_ns ip addr add 172.16.0.1/30 dev tap0
in_ns ip link set tap0 up

# Helpers run unprivileged. They are started directly, not via in_ns: ip
# netns exec and setpriv exec the next program, so $! is the helper itself.
# Their stdio is detached, or they would hold this script's stdout open.
as_vm_user=(setpriv --reuid=sbox-vm --regid=sbox-vm --clear-groups)

# DNS logger; port 53 is allowed in this namespace only.
in_ns sysctl -qw net.ipv4.ip_unprivileged_port_start=53
install -o sbox-vm -m 0600 /dev/null "$run/dns.log"
ip netns exec "$netns" "${as_vm_user[@]}" \
	python3 "$base/bin/sbox-dns.py" 172.16.0.1 53 "$run/dns.log" </dev/null >/dev/null 2>&1 &
pids+=($!)

# boot <jail id> <timeout> <phase> <deps drive mode: none|ro|rw>
# Boots one microVM and returns the jailer's status (124/137: timed out).
boot() {
	local jid=$1 secs=$2 phase=$3 deps=$4
	local root=$jail_base/firecracker/$jid/root
	mkdir -p "$root"
	# Same filesystem, so hard links: no copy of the kernel, the 800 MB
	# rootfs or the dependency drive.
	ln "$base/vmlinux" "$root/vmlinux"
	ln "$base/rootfs.ext4" "$root/rootfs.ext4"
	ln "$run/drive.ext4" "$root/drive.ext4"
	local extra=""
	if [[ $deps != none ]]; then
		ln "$run/deps.ext4" "$root/deps.ext4"
		local ro=true
		[[ $deps == rw ]] && ro=false
		extra=", {\"drive_id\": \"deps\", \"path_on_host\": \"/deps.ext4\", \"is_root_device\": false, \"is_read_only\": $ro}"
	fi
	cat >"$root/vm.json" <<EOF
{
  "boot-source": {
    "kernel_image_path": "/vmlinux",
    "boot_args": "console=ttyS0 reboot=k panic=1 quiet loglevel=1 ro init=/sbin/sbox-init sbox.phase=$phase"
  },
  "drives": [
    {"drive_id": "rootfs", "path_on_host": "/rootfs.ext4", "is_root_device": true, "is_read_only": true},
    {"drive_id": "run", "path_on_host": "/drive.ext4", "is_root_device": false, "is_read_only": true}$extra
  ],
  "network-interfaces": [{"iface_id": "eth0", "host_dev_name": "tap0", "guest_mac": "06:00:ac:10:00:02"}],
  "machine-config": {"vcpu_count": 2, "mem_size_mib": 1024}
}
EOF
	local status=0
	# </dev/null: the serial console reads stdin.
	timeout --kill-after=5 "$secs" "$base/bin/jailer" \
		--id "$jid" \
		--exec-file "$base/bin/firecracker" \
		--uid "$(id -u sbox-vm)" --gid "$(id -g sbox-vm)" \
		--chroot-base-dir "$jail_base" \
		--cgroup-version 2 \
		--netns "/var/run/netns/$netns" \
		-- --no-api --config-file /vm.json </dev/null || status=$?
	return "$status"
}

deps=none
if [[ -f $run/drive/sbox/prefetch.sh && $prefetch_timeout -gt 0 ]]; then
	truncate -s 4G "$run/deps.ext4"
	mkfs.ext4 -q -F "$run/deps.ext4"
	chown sbox-vm "$run/deps.ext4"

	# Registry-only proxy in the VM's main namespace, on a unix socket, and a
	# relay to it inside the run's namespace.
	install -d -o sbox-vm -m 0700 "$run/proxy"
	install -o sbox-vm -m 0600 /dev/null "$run/proxy.log"
	"${as_vm_user[@]}" python3 "$base/bin/sbox-proxy.py" serve "$run/proxy/sock" "$run/proxy.log" </dev/null >/dev/null 2>&1 &
	proxy_pid=$!
	for _ in $(seq 50); do [[ -S $run/proxy/sock ]] && break; sleep 0.1; done
	ip netns exec "$netns" "${as_vm_user[@]}" \
		python3 "$base/bin/sbox-proxy.py" relay 172.16.0.1 3128 "$run/proxy/sock" </dev/null >/dev/null 2>&1 &
	relay_pid=$!
	pids+=("$proxy_pid" "$relay_pid")

	pre_status=0
	boot "$id-pre" "$prefetch_timeout" prefetch rw || pre_status=$?

	# No way out for the main microVM.
	kill "$relay_pid" "$proxy_pid" 2>/dev/null
	wait "$relay_pid" "$proxy_pid" 2>/dev/null || true
	[[ $pre_status == 124 || $pre_status == 137 ]] && echo "SBOX-$nonce PREFETCH-TIMEOUT"
	sed "s/^/SBOX-$nonce FETCH /" "$run/proxy.log"
	deps=ro
fi

# Exit 124 on timeout, 0 otherwise: the console (stdout) belongs to the
# guest, so the timeout is reported out of its reach.
status=0
boot "$id" "$timeout" main "$deps" || status=$?

# The guests are gone, so they can't interleave forged lines.
sed "s/^/SBOX-$nonce DNS /" "$run/dns.log"

if [[ $status == 124 || $status == 137 ]]; then
	exit 124
fi
exit 0
