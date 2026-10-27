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
# Egress: if the run carries sbox/egress (host:port per line), the main
# microVM gets the same relay and proxy, tunnelling to those hosts only.
#
# Sessions: `start` runs the same job in the background and returns once
# the guest is up; the microVM then lives until `stop` or until the guest
# powers off. Each session has a folder under $state/sessions/<id>.
#
# Usage (as root): sbox-run <run-id> <timeout-seconds> [prefetch-timeout-seconds]
#                  sbox-run start <session-id> [timeout-seconds]   (run tar on stdin)
#                  sbox-run ps
#                  sbox-run stop <session-id>
#                  sbox-run pause|resume|log <session-id>
set -euo pipefail

# Roots can be moved for tests; sudo resets the environment, so callers
# can't move them.
base=${SBOX_BASE:-/opt/sbox}
jail_base=${SBOX_JAIL:-/srv/sbox-jail}
state=${SBOX_STATE:-/var/lib/sbox}
sessions=$state/sessions

# session_ps prints "<id> <state> <started>" per session: running or
# paused while its process lives, exited once it is gone.
session_ps() {
	local dir st
	for dir in "$sessions"/*/; do
		[[ -f $dir/pid ]] || continue
		st=$(cat "$dir/state")
		kill -0 "$(cat "$dir/pid")" 2>/dev/null || st=exited
		echo "$(basename "$dir") $st $(cat "$dir/started")"
	done
}

# teardown removes what a run leaves on the VM side: anything still in its
# namespace, the namespace, its run folder, jails and cgroups. Safe to
# repeat.
teardown() {
	local id=$1
	ip netns pids "sbox-$id" 2>/dev/null | xargs -r kill -KILL 2>/dev/null || true
	ip netns del "sbox-$id" 2>/dev/null || true
	rm -rf "$state/runs/$id" "$jail_base/firecracker/$id" "$jail_base/firecracker/$id-pre"
	rmdir "/sys/fs/cgroup/firecracker/$id" "/sys/fs/cgroup/firecracker/$id-pre" 2>/dev/null || true
}

valid_id() {
	[[ ${1:-} =~ ^[a-f0-9]{8,32}$ ]] || { echo "sbox-run: bad session id" >&2; exit 2; }
}

# session_dir prints a session's folder, or fails when there is none.
session_dir() {
	valid_id "$1"
	[[ -d $sessions/$1 ]] || { echo "sbox-run: no session $1" >&2; exit 1; }
	echo "$sessions/$1"
}

# session_start keeps the run on disk, runs this script on it in session
# mode in a new process group (so stop can end all of it), and returns once
# the guest's START marker, with the run's nonce, reaches the console log.
# Sessions started more than a day ago are stopped first, so a forgotten
# one doesn't hold memory forever.
session_start() {
	local id=${1:-} secs=${2:-0} dir nonce i old now
	valid_id "$id"
	[[ $secs =~ ^[0-9]{1,5}$ ]] || { echo "sbox-run: bad timeout" >&2; exit 2; }
	now=$(date +%s)
	for old in "$sessions"/*/; do
		[[ -f $old/started ]] || continue
		if (($(cat "$old/started") < now - 86400)); then
			session_stop "$(basename "$old")"
		fi
	done
	dir=$sessions/$id
	[[ ! -e $dir ]] || { echo "sbox-run: session $id exists" >&2; exit 1; }
	mkdir -p "$dir"
	cat >"$dir/run.tar"
	nonce=$(tar -xOf "$dir/run.tar" sbox/nonce)
	date +%s >"$dir/started"
	echo running >"$dir/state"
	SBOX_SESSION=1 setsid "${BASH:-bash}" "$0" "$id" "$secs" <"$dir/run.tar" >"$dir/console.log" 2>&1 &
	echo $! >"$dir/pid"
	for ((i = 0; i < 1200; i++)); do # 120 s
		if grep -q "SBOX-$nonce START" "$dir/console.log"; then
			rm -f "$dir/run.tar"
			echo "READY $id"
			return 0
		fi
		kill -0 "$(cat "$dir/pid")" 2>/dev/null || break
		sleep 0.1
	done
	echo "sbox-run: session $id did not start" >&2
	tail -n 20 "$dir/console.log" >&2
	kill -KILL -- "-$(cat "$dir/pid")" 2>/dev/null || true
	rm -rf "$dir"
	return 1
}

# session_stop ends a session's process group: TERM first, so the run's own
# cleanup stops its helpers; KILL after 10 s. The teardown then runs here
# too, in case that cleanup never got to.
session_stop() {
	local id=${1:-} dir pid i
	dir=$(session_dir "$id")
	pid=$(cat "$dir/pid")
	if kill -0 "$pid" 2>/dev/null; then
		kill -TERM -- "-$pid" 2>/dev/null || true
		for ((i = 0; i < 100; i++)); do
			kill -0 "$pid" 2>/dev/null || break
			sleep 0.1
		done
		kill -KILL -- "-$pid" 2>/dev/null || true
	fi
	teardown "$id"
	rm -rf "$dir"
}

# session_vm_state moves a live session from one state to another through
# Firecracker's API (PATCH /vm), and records the new state only once
# Firecracker has accepted it.
session_vm_state() {
	local id=$1 from=$2 to=$3 vm_state=$4 dir
	dir=$(session_dir "$id")
	if ! kill -0 "$(cat "$dir/pid")" 2>/dev/null || [[ $(cat "$dir/state") != "$from" ]]; then
		echo "sbox-run: session $id is not $from" >&2
		exit 1
	fi
	curl -fsS --unix-socket "$jail_base/firecracker/$id/root/run/firecracker.socket" \
		-X PATCH -H 'Content-Type: application/json' -d "{\"state\":\"$vm_state\"}" http://localhost/vm ||
		{ echo "sbox-run: Firecracker did not change session $id to $to" >&2; exit 1; }
	echo "$to" >"$dir/state"
}

case ${1:-} in
ps) session_ps; exit 0 ;;
log) dir=$(session_dir "${2:-}"); cat "$dir/console.log"; exit 0 ;;
pause) session_vm_state "${2:-}" running paused Paused; exit 0 ;;
resume) session_vm_state "${2:-}" paused running Resumed; exit 0 ;;
stop) shift; session_stop "$@"; exit 0 ;;
start) shift; session_start "$@"; exit 0 ;;
esac

id=${1:-} timeout=${2:-} prefetch_timeout=${3:-0}
[[ $id =~ ^[a-f0-9]{8,32}$ ]] || { echo "sbox-run: bad run id" >&2; exit 2; }
[[ $timeout =~ ^[0-9]{1,5}$ ]] || { echo "sbox-run: bad timeout" >&2; exit 2; }
[[ $prefetch_timeout =~ ^[0-9]{1,5}$ ]] || { echo "sbox-run: bad prefetch timeout" >&2; exit 2; }

run=$state/runs/$id
netns=sbox-$id

pids=()
cleanup() {
	# Some are already stopped; a failed kill must not abort the cleanup.
	((${#pids[@]})) && kill "${pids[@]}" 2>/dev/null || true
	# Anything else still in the run's namespace goes too, so nothing a run
	# starts outlives it.
	teardown "$id"
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
	# Sessions are paused and resumed through Firecracker's API; one-shot
	# runs need no API.
	local api=(--no-api)
	if [[ ${SBOX_SESSION:-} == 1 && $phase == main ]]; then
		mkdir -p "$root/run"
		chown sbox-vm "$root/run"
		api=(--api-sock /run/firecracker.socket)
	fi
	local status=0
	# </dev/null: the serial console reads stdin.
	timeout --kill-after=5 "$secs" "$base/bin/jailer" \
		--id "$jid" \
		--exec-file "$base/bin/firecracker" \
		--uid "$(id -u sbox-vm)" --gid "$(id -g sbox-vm)" \
		--chroot-base-dir "$jail_base" \
		--cgroup-version 2 \
		--netns "/var/run/netns/$netns" \
		-- "${api[@]}" --config-file /vm.json </dev/null || status=$?
	return "$status"
}

# start_egress <log> [host:port ...]: an allowlist proxy in the VM's main
# namespace on a unix socket (default allowlist: the package registries),
# and a relay to it on 172.16.0.1:3128 inside the run's namespace.
start_egress() {
	local log=$1
	shift
	install -d -o sbox-vm -m 0700 "$run/proxy"
	rm -f "$run/proxy/sock"
	install -o sbox-vm -m 0600 /dev/null "$log"
	"${as_vm_user[@]}" python3 "$base/bin/sbox-proxy.py" serve "$run/proxy/sock" "$log" "$@" </dev/null >/dev/null 2>&1 &
	proxy_pid=$!
	for _ in $(seq 50); do [[ -S $run/proxy/sock ]] && break; sleep 0.1; done
	ip netns exec "$netns" "${as_vm_user[@]}" \
		python3 "$base/bin/sbox-proxy.py" relay 172.16.0.1 3128 "$run/proxy/sock" </dev/null >/dev/null 2>&1 &
	relay_pid=$!
	pids+=("$proxy_pid" "$relay_pid")
}

stop_egress() {
	kill "$relay_pid" "$proxy_pid" 2>/dev/null || true
	wait "$relay_pid" "$proxy_pid" 2>/dev/null || true
}

deps=none
if [[ -f $run/drive/sbox/prefetch.sh && $prefetch_timeout -gt 0 ]]; then
	truncate -s 4G "$run/deps.ext4"
	mkfs.ext4 -q -F "$run/deps.ext4"
	chown sbox-vm "$run/deps.ext4"

	start_egress "$run/prefetch.log"
	pre_status=0
	boot "$id-pre" "$prefetch_timeout" prefetch rw || pre_status=$?
	stop_egress # no way out for the main microVM, unless it declares egress

	[[ $pre_status == 124 || $pre_status == 137 ]] && echo "SBOX-$nonce PREFETCH-TIMEOUT"
	sed "s/^/SBOX-$nonce FETCH /" "$run/prefetch.log"
	deps=ro
fi

egress=()
if [[ -s $run/drive/sbox/egress ]]; then
	while read -r entry; do
		[[ $entry =~ ^[a-z0-9.-]+:[0-9]{1,5}$ ]] && egress+=("$entry")
	done <"$run/drive/sbox/egress"
fi
((${#egress[@]})) && start_egress "$run/egress.log" "${egress[@]}"

# Exit 124 on timeout, 0 otherwise: the console (stdout) belongs to the
# guest, so the timeout is reported out of its reach.
status=0
boot "$id" "$timeout" main "$deps" || status=$?

if ((${#egress[@]})); then
	stop_egress
	sed "s/^/SBOX-$nonce EGRESS /" "$run/egress.log"
fi

# The guests are gone, so they can't interleave forged lines.
sed "s/^/SBOX-$nonce DNS /" "$run/dns.log"

if [[ $status == 124 || $status == 137 ]]; then
	exit 124
fi
exit 0
